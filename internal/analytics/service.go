// Package analytics records privacy-preserving local page-view statistics.
package analytics

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

const (
	flushInterval = 10 * time.Second
	bufferSize    = 4096
)

var botUserAgent = regexp.MustCompile(`(?i)(bot|crawler|spider|crawl|slurp|Googlebot|BingPreview|DuckDuckBot|YandexBot|Baiduspider|facebookexternalhit|Twitterbot|curl|wget|python-requests|Go-http-client|HeadlessChrome)`)

// Stats is a site's or article's daily aggregate.
type Stats struct {
	Date           string `db:"date" json:"date"`
	Views          int64  `db:"views" json:"views"`
	UniqueVisitors int64  `db:"unique_visitors" json:"unique_visitors"`
}

// PopularArticle contains an article identifier and its aggregate page views.
type PopularArticle struct {
	ArticleID      string `db:"article_id" json:"article_id"`
	Views          int64  `db:"views" json:"views"`
	UniqueVisitors int64  `db:"unique_visitors" json:"unique_visitors"`
}

type event struct {
	articleID string
	date      string
	visitor   string
}
type aggregate struct{ views, unique int64 }

// Service batches page views in memory and persists only daily aggregates.
// Daily dates are calendar days in the service's location (see WithLocation).
type Service struct {
	db       *sqlx.DB
	loc      *time.Location
	now      func() time.Time
	mu       sync.Mutex // protects closed, salt rotation, and channel sends
	closed   bool
	day      string
	salt     []byte
	events   chan event
	stop     chan struct{}
	done     chan struct{}
	closeErr error
}

// Option configures a Service.
type Option func(*Service)

// WithLocation sets the time zone whose calendar days bound the daily aggregates.
// A nil location means time.Local (the default).
func WithLocation(loc *time.Location) Option {
	return func(s *Service) { s.loc = loc }
}

// WithClock sets the function that returns the current time (the default is time.Now).
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// NewService starts a service that flushes pending aggregates every ten seconds.
func NewService(db *sqlx.DB, opts ...Option) *Service {
	s := &Service{db: db, events: make(chan event, bufferSize), stop: make(chan struct{}), done: make(chan struct{})}
	for _, opt := range opts {
		opt(s)
	}
	if s.loc == nil {
		s.loc = time.Local
	}
	if s.now == nil {
		s.now = time.Now
	}
	_ = s.rotateSalt(s.now())
	go s.run()
	return s
}

// Location returns the time zone whose calendar days bound the daily aggregates.
func (s *Service) Location() *time.Location { return s.loc }

// date returns the aggregate date (YYYY-MM-DD) of t in the service's location.
func (s *Service) date(t time.Time) string { return t.In(s.loc).Format("2006-01-02") }

// period returns the first and last dates of the trailing days that end today (inclusive).
// Rows dated after today (e.g. recorded before a time zone change) are left out.
func (s *Service) period(days int) (from, to string) {
	today := s.now().In(s.loc)
	return s.date(today.AddDate(0, 0, -(days - 1))), s.date(today)
}

// rotateSalt replaces the visitor-hash salt when the local day changes, so a visitor is
// counted once per aggregate date and hashes cannot be linked across days.
func (s *Service) rotateSalt(now time.Time) error {
	day := s.date(now)
	if s.day == day && len(s.salt) != 0 {
		return nil
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("analytics: generate daily salt: %w", err)
	}
	s.day, s.salt = day, salt
	return nil
}

// RecordView queues a page view. It returns false for bots, blank article IDs,
// a closed service, or a full buffer; accepted views are persisted asynchronously.
func (s *Service) RecordView(articleID, ip, userAgent string) bool {
	if strings.TrimSpace(articleID) == "" || botUserAgent.MatchString(userAgent) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.rotateSalt(s.now()) != nil {
		return false
	}
	mac := hmac.New(sha256.New, s.salt)
	_, _ = mac.Write([]byte(ip + "\x00" + userAgent))
	e := event{articleID: articleID, date: s.day, visitor: string(mac.Sum(nil))}
	select {
	case s.events <- e:
		return true
	default:
		return false
	}
}

// TrackView is an alias for RecordView.
func (s *Service) TrackView(articleID, ip, userAgent string) bool {
	return s.RecordView(articleID, ip, userAgent)
}

func (s *Service) run() {
	defer close(s.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	articleTotals := make(map[string]aggregate)
	siteTotals := make(map[string]aggregate)
	articleUnique := make(map[string]map[string]struct{})
	siteUnique := make(map[string]map[string]struct{})
	flush := func() error {
		if len(articleTotals) == 0 && len(siteTotals) == 0 {
			return nil
		}
		tx, err := s.db.Beginx()
		if err != nil {
			return fmt.Errorf("analytics: begin flush: %w", err)
		}
		for key, a := range articleTotals {
			date, id, _ := strings.Cut(key, "\x00")
			// Skip articles removed since the view was queued (e.g. by a backup restore); a foreign key
			// failure would otherwise abort every later flush.
			if _, err = tx.Exec(`INSERT INTO article_views_daily (article_id, date, views, unique_visitors)
				SELECT ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM articles WHERE id = ?)
				ON CONFLICT(article_id, date) DO UPDATE SET views = views + excluded.views, unique_visitors = unique_visitors + excluded.unique_visitors`, id, date, a.views, a.unique, id); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("analytics: flush article views: %w", err)
			}
		}
		for date, a := range siteTotals {
			if _, err = tx.Exec(`INSERT INTO site_views_daily (date, views, unique_visitors) VALUES (?, ?, ?)
				ON CONFLICT(date) DO UPDATE SET views = views + excluded.views, unique_visitors = unique_visitors + excluded.unique_visitors`, date, a.views, a.unique); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("analytics: flush site views: %w", err)
			}
		}
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("analytics: commit flush: %w", err)
		}
		clear(articleTotals)
		clear(siteTotals)
		return nil
	}
	consume := func(e event) {
		ak := e.date + "\x00" + e.articleID
		a := articleTotals[ak]
		a.views++
		if articleUnique[ak] == nil {
			articleUnique[ak] = make(map[string]struct{})
		}
		if _, exists := articleUnique[ak][e.visitor]; !exists {
			articleUnique[ak][e.visitor] = struct{}{}
			a.unique++
		}
		articleTotals[ak] = a
		s := siteTotals[e.date]
		s.views++
		if siteUnique[e.date] == nil {
			siteUnique[e.date] = make(map[string]struct{})
		}
		if _, exists := siteUnique[e.date][e.visitor]; !exists {
			siteUnique[e.date][e.visitor] = struct{}{}
			s.unique++
		}
		siteTotals[e.date] = s
	}
	for {
		select {
		case e := <-s.events:
			consume(e)
		case <-ticker.C:
			if err := flush(); err != nil {
				s.rememberError(err)
			}
		case <-s.stop:
			for {
				select {
				case e := <-s.events:
					consume(e)
				default:
					goto drained
				}
			}
		drained:
			if err := flush(); err != nil {
				s.rememberError(err)
			}
			return
		}
	}
}
func (s *Service) rememberError(err error) {
	s.mu.Lock()
	s.closeErr = errors.Join(s.closeErr, err)
	s.mu.Unlock()
}

// Close stops accepting views and synchronously flushes all queued aggregates.
func (s *Service) Close() error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.stop)
	}
	s.mu.Unlock()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeErr
}

// Shutdown is an alias for Close.
func (s *Service) Shutdown() error { return s.Close() }

// GetArticleViews returns the sum of daily article statistics.
func (s *Service) GetArticleViews(id string) (Stats, error) {
	var result Stats
	result.Date = "all"
	err := s.db.Get(&result, `SELECT COALESCE(SUM(views), 0) AS views, COALESCE(SUM(unique_visitors), 0) AS unique_visitors FROM article_views_daily WHERE article_id = ?`, id)
	if err != nil {
		return Stats{}, fmt.Errorf("analytics: get article views: %w", err)
	}
	return result, nil
}

// GetPopularArticles returns the most viewed articles in the trailing days (local dates, inclusive).
func (s *Service) GetPopularArticles(limit, days int) ([]PopularArticle, error) {
	if limit <= 0 || days <= 0 {
		return []PopularArticle{}, nil
	}
	from, to := s.period(days)
	var result []PopularArticle
	err := s.db.Select(&result, `SELECT article_id, SUM(views) AS views, SUM(unique_visitors) AS unique_visitors FROM article_views_daily WHERE date BETWEEN ? AND ? GROUP BY article_id ORDER BY views DESC, article_id LIMIT ?`, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("analytics: get popular articles: %w", err)
	}
	return result, nil
}

// GetSiteStats returns daily site totals in the trailing days (local dates, inclusive), newest first.
func (s *Service) GetSiteStats(days int) ([]Stats, error) {
	if days <= 0 {
		return []Stats{}, nil
	}
	from, to := s.period(days)
	var result []Stats
	err := s.db.Select(&result, `SELECT date, views, unique_visitors FROM site_views_daily WHERE date BETWEEN ? AND ? ORDER BY date DESC LIMIT ?`, from, to, days)
	if errors.Is(err, sql.ErrNoRows) {
		return []Stats{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("analytics: get site stats: %w", err)
	}
	return result, nil
}
