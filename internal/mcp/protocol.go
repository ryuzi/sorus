package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
)

// jsonrpcVersion は JSON-RPC のバージョン。リクエストの "jsonrpc" はこの値でなければならない。
const jsonrpcVersion = "2.0"

// JSON-RPC 2.0 の定義済みエラーコードと、MCP が使うエラーコード。
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	// CodeResourceNotFound は resources/read で URI のリソースが無いことを表す（MCP の取り決め）。
	CodeResourceNotFound = -32002
)

// LatestProtocolVersion はサーバーが対応する最新の MCP プロトコルバージョン。
// クライアントが対応外のバージョンを要求したときは、これを返してクライアントに判断を委ねる。
const LatestProtocolVersion = "2025-11-25"

// supportedProtocolVersions は対応する MCP プロトコルバージョン。提供する機能（tools・resources）は
// どのバージョンでも同じ形でやり取りできる。
var supportedProtocolVersions = []string{LatestProtocolVersion, "2025-06-18", "2025-03-26", "2024-11-05"}

// negotiateVersion は initialize で要求されたバージョンに対応していればそれを、していなければ最新を返す。
func negotiateVersion(requested string) string {
	if slices.Contains(supportedProtocolVersions, requested) {
		return requested
	}
	return LatestProtocolVersion
}

// errParse は本文が JSON として解釈できないことを表す。
var errParse = errors.New("mcp: parse error")

// rpcError は JSON-RPC 2.0 の Error オブジェクト。
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

func newError(code int, msg string) *rpcError { return &rpcError{Code: code, Message: msg} }

// response は JSON-RPC 2.0 の Response オブジェクト。MarshalJSON は result と error のどちらか一方だけを書き出す。
type response struct {
	// ID はリクエストの id をそのまま返す。id を特定できなかったときは nil で、null を書き出す。
	ID     json.RawMessage
	Result any
	Error  *rpcError
}

func (r *response) MarshalJSON() ([]byte, error) {
	id := r.ID
	if id == nil {
		id = json.RawMessage("null")
	}
	if r.Error != nil {
		return marshalJSON(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Error   *rpcError       `json:"error"`
		}{jsonrpcVersion, id, r.Error}, false)
	}
	result := r.Result
	if result == nil {
		result = struct{}{}
	}
	return marshalJSON(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{jsonrpcVersion, id, result}, false)
}

// marshalJSON は v を JSON にする。json.Marshal と違い < > & を \u003c などにエスケープしない
// （HTML に埋め込まない応答で、AI が読む記事本文を読みやすく保つため）。indent なら2文字で字下げする。
// indent でなければ改行を含まないため、SSE の data 行にそのまま書ける。
func marshalJSON(v any, indent bool) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// request は検証済みの JSON-RPC リクエストまたは通知。
type request struct {
	// ID は通知なら nil。null の id（非推奨だが JSON-RPC 2.0 では有効）は "null" になる。
	ID     json.RawMessage
	Method string
	// Params は省略されていれば nil。存在すればオブジェクトか配列。
	Params json.RawMessage
}

func (r *request) isNotification() bool { return r.ID == nil }

// splitBatch は POST の本文を JSON-RPC のメッセージに分ける。本文が配列ならバッチとして要素ごとに返す。
// JSON として解釈できなければ errParse を返す。
func splitBatch(body []byte) (msgs []json.RawMessage, batch bool, err error) {
	body = bytes.TrimSpace(body)
	if !json.Valid(body) {
		return nil, false, errParse
	}
	if body[0] != '[' {
		return []json.RawMessage{body}, false, nil
	}
	if err := json.Unmarshal(body, &msgs); err != nil {
		return nil, false, errParse
	}
	return msgs, true, nil
}

// parseMessage は1件のメッセージを検証する。
//
// 有効なリクエスト・通知なら req を返す。クライアントからのレスポンス（result か error を持ち method が無い）は
// サーバーから要求を送らないため読み捨て、req と errResp の両方が nil になる。それ以外の不正なメッセージには
// Invalid Request のレスポンスを返す。id が有効に読み取れればその id で、読み取れなければ null で応答する。
func parseMessage(raw json.RawMessage) (req *request, errResp *response) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, &response{Error: newError(CodeInvalidRequest, "invalid request: message must be a JSON object")}
	}
	id, hasID := fields["id"]
	if hasID && !validID(id) {
		return nil, &response{Error: newError(CodeInvalidRequest, "invalid request: id must be a string, number or null")}
	}
	var replyID json.RawMessage
	if hasID {
		replyID = id
	}
	invalid := func(msg string) (*request, *response) {
		return nil, &response{ID: replyID, Error: newError(CodeInvalidRequest, "invalid request: "+msg)}
	}

	var version string
	if v, ok := fields["jsonrpc"]; !ok || json.Unmarshal(v, &version) != nil || version != jsonrpcVersion {
		return invalid(`"jsonrpc" must be "2.0"`)
	}
	rawMethod, ok := fields["method"]
	if !ok {
		_, hasResult := fields["result"]
		_, hasError := fields["error"]
		if hasID && hasResult != hasError {
			return nil, nil
		}
		return invalid(`"method" is required`)
	}
	var method string
	if json.Unmarshal(rawMethod, &method) != nil {
		return invalid(`"method" must be a string`)
	}
	params := fields["params"]
	// null の params は省略と同じに扱い、params を省略せず null で送るクライアントとも相互に動くようにする。
	if string(params) == "null" {
		params = nil
	}
	if params != nil && params[0] != '{' && params[0] != '[' {
		return invalid(`"params" must be an object or an array`)
	}
	return &request{ID: replyID, Method: method, Params: params}, nil
}

// validID は id が文字列・数値・null のいずれかかを返す。
func validID(raw json.RawMessage) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	switch v.(type) {
	case string, float64, nil:
		return true
	}
	return false
}

// decodeParams は params を v に読み込む。省略されていれば v を変えない。
// オブジェクトとして読めなければ Invalid params のエラーを返す。
func decodeParams(params json.RawMessage, v any) *rpcError {
	if params == nil {
		return nil
	}
	if params[0] != '{' {
		return newError(CodeInvalidParams, "invalid params: params must be an object")
	}
	if err := json.Unmarshal(params, v); err != nil {
		return newError(CodeInvalidParams, "invalid params: "+err.Error())
	}
	return nil
}

// implementation は initialize で交換するクライアント・サーバーの名前とバージョン。
type implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      implementation `json:"clientInfo"`
}

type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      implementation     `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

// serverCapabilities はサーバーが提供する機能。ツール・リソースの一覧は実行中に変わらない。
type serverCapabilities struct {
	Tools     *toolsCapability     `json:"tools,omitempty"`
	Resources *resourcesCapability `json:"resources,omitempty"`
}

type toolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

type resourcesCapability struct {
	Subscribe   bool `json:"subscribe"`
	ListChanged bool `json:"listChanged"`
}

// toolInfo は tools/list が返すツールの定義。
type toolInfo struct {
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description"`
	InputSchema map[string]any   `json:"inputSchema"`
	Annotations *toolAnnotations `json:"annotations,omitempty"`
}

// toolAnnotations はクライアントが確認ダイアログなどの判断に使うツールの性質。
// 省略時の既定値（destructiveHint=true など）に頼らず、すべて明示する。
type toolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint"`
	DestructiveHint bool   `json:"destructiveHint"`
	IdempotentHint  bool   `json:"idempotentHint"`
	OpenWorldHint   bool   `json:"openWorldHint"`
}

type listToolsResult struct {
	Tools []toolInfo `json:"tools"`
}

type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// content はツール結果の本文。このサーバーは text だけを返す。
type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// callToolResult は tools/call の結果。ツールの実行時エラー（権限不足・入力不備など）は JSON-RPC のエラーではなく
// IsError=true の結果として返し、AI が内容を読んで対処できるようにする。
type callToolResult struct {
	Content []content `json:"content"`
	// StructuredContent は Content の JSON と同じ値。2025-06-18 以降のクライアントが機械的に読む。
	StructuredContent any  `json:"structuredContent,omitempty"`
	IsError           bool `json:"isError"`
}

type resourceInfo struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

type resourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

type paginatedParams struct {
	Cursor string `json:"cursor"`
}

type listResourcesResult struct {
	Resources  []resourceInfo `json:"resources"`
	NextCursor string         `json:"nextCursor,omitempty"`
}

type listResourceTemplatesResult struct {
	ResourceTemplates []resourceTemplate `json:"resourceTemplates"`
}

type readResourceParams struct {
	URI string `json:"uri"`
}

type resourceContents struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text"`
}

type readResourceResult struct {
	Contents []resourceContents `json:"contents"`
}
