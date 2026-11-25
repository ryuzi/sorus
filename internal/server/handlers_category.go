package server

import (
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"sorus/internal/article"
	"sorus/internal/i18n"
)

const (
	categorySettingsPath = "/admin/settings/categories"
	maxCategoryNameRunes = 100
	maxCategoryDescRunes = 1000
)

func init() {
	if !slices.Contains(pageNames, "settings_categories") {
		pageNames = append(pageNames, "settings_categories")
	}
	if !slices.Contains(pageNames, "settings_category_edit") {
		pageNames = append(pageNames, "settings_category_edit")
	}
	flashMessages["category_created"] = "カテゴリを追加しました"
	flashMessages["category_saved"] = "カテゴリを保存しました"
	flashMessages["category_deleted"] = "カテゴリを削除しました"
	for _, action := range []string{"category_saved", "category_deleted"} {
		flashMessages[action+"_reflecting"] = flashMessages[action] + "。公開サイトへの自動反映を開始しました"
		flashMessages[action+"_manual"] = flashMessages[action] + "。静的サイトへ反映するには『サイトに反映』を実行してください"
		flashMessages[action+"_no_target"] = flashMessages[action] + "。公開先設定画面で公開先を設定してください"
		for _, outcome := range []string{"reflecting", "manual", "no_target"} {
			key := action + "_" + outcome
			flashKeys[key] = key
		}
	}
}

type managedCategory struct {
	ID          int64  `db:"id"`
	Name        string `db:"name"`
	Slug        string `db:"slug"`
	Description string `db:"description"`
}

type categoryForm struct {
	Name, Slug, Description string
}

type categorySettingsData struct {
	Categories   []managedCategory
	Form         categoryForm
	Errors       []string
	DefaultError bool
}

type categoryEditData struct {
	Category managedCategory
	Form     categoryForm
	Errors   []string
}

func (s *Server) registerCategoryRoutes(authed *echo.Group, editorOrAdmin echo.MiddlewareFunc) {
	authed.GET("/settings/categories", s.handleCategories, editorOrAdmin)
	authed.POST("/settings/categories", s.handleCategoryCreate, editorOrAdmin)
	authed.GET("/settings/categories/:id/edit", s.handleCategoryEdit, editorOrAdmin)
	authed.POST("/settings/categories/:id/edit", s.handleCategorySave, editorOrAdmin)
	authed.POST("/settings/categories/:id/delete", s.handleCategoryDelete, editorOrAdmin)
}

func (s *Server) handleCategories(c echo.Context) error {
	var categories []managedCategory
	if err := s.db.Select(&categories, `SELECT id, name, slug, description FROM categories ORDER BY id`); err != nil {
		return err
	}
	return s.render(c, http.StatusOK, "settings_categories", i18n.Translate(string(language(c)), "categories"), categorySettingsData{
		Categories: categories, DefaultError: c.QueryParam("error") == "category_default",
	})
}

func (s *Server) handleCategoryCreate(c echo.Context) error {
	form := categoryForm{
		Name: strings.TrimSpace(c.FormValue("name")), Slug: strings.TrimSpace(c.FormValue("slug")),
		Description: strings.TrimSpace(c.FormValue("description")),
	}
	if errs := validateCategoryForm(form); len(errs) > 0 {
		return s.render(c, http.StatusBadRequest, "settings_categories", i18n.Translate(string(language(c)), "categories"), categorySettingsData{Form: form, Errors: categoryErrors(c, errs)})
	}
	if err := s.ensureCategoryValuesAvailable(form, 0); err != nil {
		if errors.Is(err, errCategoryNameTaken) || errors.Is(err, errCategorySlugTaken) {
			key := "category_err_name_taken"
			if errors.Is(err, errCategorySlugTaken) {
				key = "category_err_slug_taken"
			}
			return s.render(c, http.StatusConflict, "settings_categories", i18n.Translate(string(language(c)), "categories"), categorySettingsData{Form: form, Errors: categoryErrors(c, []string{key})})
		}
		return err
	}
	if _, err := s.db.ExecContext(c.Request().Context(), `INSERT INTO categories (name, slug, description) VALUES (?, ?, ?)`, form.Name, form.Slug, form.Description); err != nil {
		return err
	}
	return c.Redirect(http.StatusSeeOther, categorySettingsPath+"?flash=category_created")
}

func (s *Server) handleCategoryEdit(c echo.Context) error {
	cat, err := s.categoryByID(c)
	if err != nil {
		return err
	}
	form := categoryForm{cat.Name, cat.Slug, cat.Description}
	return s.render(c, http.StatusOK, "settings_category_edit", i18n.Translate(string(language(c)), "category_edit"), categoryEditData{Category: cat, Form: form})
}

func (s *Server) handleCategorySave(c echo.Context) error {
	cat, err := s.categoryByID(c)
	if err != nil {
		return err
	}
	form := categoryForm{
		Name: strings.TrimSpace(c.FormValue("name")), Slug: strings.TrimSpace(c.FormValue("slug")),
		Description: strings.TrimSpace(c.FormValue("description")),
	}
	errs := validateCategoryForm(form)
	if len(errs) > 0 {
		return s.render(c, http.StatusBadRequest, "settings_category_edit", i18n.Translate(string(language(c)), "category_edit"), categoryEditData{Category: cat, Form: form, Errors: categoryErrors(c, errs)})
	}
	if err := s.ensureCategoryValuesAvailable(form, cat.ID); err != nil {
		if errors.Is(err, errCategoryNameTaken) || errors.Is(err, errCategorySlugTaken) {
			key := "category_err_name_taken"
			if errors.Is(err, errCategorySlugTaken) {
				key = "category_err_slug_taken"
			}
			return s.render(c, http.StatusConflict, "settings_category_edit", i18n.Translate(string(language(c)), "category_edit"), categoryEditData{Category: cat, Form: form, Errors: categoryErrors(c, []string{key})})
		}
		return err
	}
	var publishedCount int
	visible, visibleArgs := article.VisibleCondition, []any{article.StatusPublished, clockNow()}
	if err := s.db.GetContext(c.Request().Context(), &publishedCount, `SELECT COUNT(*) FROM articles a WHERE a.category_id = ? AND `+visible, append([]any{cat.ID}, visibleArgs...)...); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(c.Request().Context(), `UPDATE categories SET name = ?, slug = ?, description = ? WHERE id = ?`, form.Name, form.Slug, form.Description, cat.ID); err != nil {
		return err
	}
	flash := "category_saved"
	if publishedCount > 0 {
		flash = categoryReflectFlash(flash, s.reflectAfterPublish())
	}
	return c.Redirect(http.StatusSeeOther, categorySettingsPath+"?flash="+flash)
}

func (s *Server) handleCategoryDelete(c echo.Context) error {
	cat, err := s.categoryByID(c)
	if err != nil {
		return err
	}
	if isUncategorized(cat) {
		return c.Redirect(http.StatusSeeOther, categorySettingsPath+"?error=category_default")
	}
	tx, err := s.db.BeginTxx(c.Request().Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var fallbackID int64
	if err := tx.GetContext(c.Request().Context(), &fallbackID, `SELECT id FROM categories WHERE id = 1 OR slug = 'uncategorized' ORDER BY slug = 'uncategorized' DESC, id LIMIT 1`); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusConflict, "default category is missing")
		}
		return err
	}
	if fallbackID == cat.ID {
		return c.Redirect(http.StatusSeeOther, categorySettingsPath+"?error=category_default")
	}
	var publishedCount int
	visible, visibleArgs := article.VisibleCondition, []any{article.StatusPublished, clockNow()}
	if err := tx.GetContext(c.Request().Context(), &publishedCount, `SELECT COUNT(*) FROM articles a WHERE a.category_id = ? AND `+visible, append([]any{cat.ID}, visibleArgs...)...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(c.Request().Context(), `UPDATE articles SET category_id = ? WHERE category_id = ?`, fallbackID, cat.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(c.Request().Context(), `DELETE FROM categories WHERE id = ?`, cat.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	flash := "category_deleted"
	if publishedCount > 0 {
		flash = categoryReflectFlash(flash, s.reflectAfterPublish())
	}
	return c.Redirect(http.StatusSeeOther, categorySettingsPath+"?flash="+flash)
}

func categoryReflectFlash(base, result string) string {
	switch result {
	case flashPublishedReflecting:
		return base + "_reflecting"
	case flashPublishedManual:
		return base + "_manual"
	case flashPublishedNoTarget:
		return base + "_no_target"
	default:
		return base
	}
}

func (s *Server) categoryByID(c echo.Context) (managedCategory, error) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		return managedCategory{}, echo.NewHTTPError(http.StatusNotFound)
	}
	var cat managedCategory
	if err := s.db.GetContext(c.Request().Context(), &cat, `SELECT id, name, slug, description FROM categories WHERE id = ?`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return managedCategory{}, echo.NewHTTPError(http.StatusNotFound)
		}
		return managedCategory{}, err
	}
	return cat, nil
}

func isUncategorized(cat managedCategory) bool { return cat.ID == 1 || cat.Slug == "uncategorized" }

var (
	errCategoryNameTaken = errors.New("category name already exists")
	errCategorySlugTaken = errors.New("category slug already exists")
)

func validateCategoryForm(form categoryForm) []string {
	var errs []string
	switch {
	case form.Name == "":
		errs = append(errs, "category_err_name_required")
	case utf8.RuneCountInString(form.Name) > maxCategoryNameRunes:
		errs = append(errs, "category_err_name_long")
	}
	switch {
	case form.Slug == "":
		errs = append(errs, "category_err_slug_required")
	case validateStructureSlug(form.Slug) != nil:
		errs = append(errs, "category_err_slug_invalid")
	}
	if utf8.RuneCountInString(form.Description) > maxCategoryDescRunes {
		errs = append(errs, "category_err_desc_long")
	}
	return errs
}

func (s *Server) ensureCategoryValuesAvailable(form categoryForm, exceptID int64) error {
	var nameCount, slugCount int
	if err := s.db.Get(&nameCount, `SELECT COUNT(*) FROM categories WHERE name = ? AND id <> ?`, form.Name, exceptID); err != nil {
		return err
	}
	if nameCount != 0 {
		return errCategoryNameTaken
	}
	if err := s.db.Get(&slugCount, `SELECT COUNT(*) FROM categories WHERE slug = ? AND id <> ?`, form.Slug, exceptID); err != nil {
		return err
	}
	if slugCount != 0 {
		return errCategorySlugTaken
	}
	return nil
}

func categoryErrors(c echo.Context, keys []string) []string {
	lang := string(language(c))
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, i18n.Translate(lang, key))
	}
	return result
}
