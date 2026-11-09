package chatwoot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// maxDownloadSize limits attachments downloaded from Chatwoot (WhatsApp's own
// limit for documents is 2 GB, but Chatwoot rejects uploads above 40 MB by default).
const maxDownloadSize = 100 << 20

// Client calls the Chatwoot application API of one account.
type Client struct {
	baseURL   *url.URL
	accountID int
	token     string
	http      *http.Client
}

// APIError is returned when Chatwoot answers with a non-2xx status.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("chatwoot %s %s: status %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// IsNotFound reports whether err is a Chatwoot 404.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

// NewClient creates a client. baseURL is the Chatwoot root, for example
// http://chatwoot-rails:3000.
func NewClient(baseURL string, accountID int, token string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid Chatwoot URL %q", baseURL)
	}
	c := &Client{baseURL: u, accountID: accountID, token: token}
	c.http = &http.Client{Timeout: 60 * time.Second, CheckRedirect: c.checkRedirect}
	return c, nil
}

type Inbox struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	ChannelType string `json:"channel_type"`
	WebhookURL  string `json:"webhook_url"`
	// Secret is only returned to administrators.
	Secret string `json:"secret"`
}

type Contact struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	PhoneNumber string `json:"phone_number"`
	Identifier  string `json:"identifier"`
	Thumbnail   string `json:"thumbnail"`
}

type Conversation struct {
	ID      int    `json:"id"`
	InboxID int    `json:"inbox_id"`
	Status  string `json:"status"`
}

type CWMessage struct {
	ID             int    `json:"id"`
	ConversationID int    `json:"conversation_id"`
	MessageType    any    `json:"message_type"`
	SourceID       string `json:"source_id"`
}

// Attachment is a file uploaded with a new message.
type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// NewMessage is a message to create in a conversation.
type NewMessage struct {
	Content     string
	MessageType string // "incoming" or "outgoing"
	SourceID    string
	Private     bool
	InReplyTo   int
	Attachment  *Attachment
}

func (c *Client) accountPath(format string, args ...any) string {
	return fmt.Sprintf("/api/v1/accounts/%d", c.accountID) + fmt.Sprintf(format, args...)
}

func (c *Client) ListInboxes(ctx context.Context) ([]Inbox, error) {
	var out struct {
		Payload []Inbox `json:"payload"`
	}
	err := c.doJSON(ctx, http.MethodGet, c.accountPath("/inboxes"), nil, &out)
	return out.Payload, err
}

func (c *Client) CreateInbox(ctx context.Context, name, webhookURL string) (*Inbox, error) {
	body := map[string]any{
		"name":    name,
		"channel": map[string]any{"type": "api", "webhook_url": webhookURL},
	}
	var out Inbox
	if err := c.doJSON(ctx, http.MethodPost, c.accountPath("/inboxes"), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) UpdateInboxWebhook(ctx context.Context, inboxID int, webhookURL string) (*Inbox, error) {
	body := map[string]any{"channel": map[string]any{"webhook_url": webhookURL}}
	var out Inbox
	if err := c.doJSON(ctx, http.MethodPatch, c.accountPath("/inboxes/%d", inboxID), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) SearchContacts(ctx context.Context, query string) ([]Contact, error) {
	var out struct {
		Payload []Contact `json:"payload"`
	}
	p := c.accountPath("/contacts/search") + "?q=" + url.QueryEscape(query)
	err := c.doJSON(ctx, http.MethodGet, p, nil, &out)
	return out.Payload, err
}

// CreateContact creates a contact already linked to the inbox.
func (c *Client) CreateContact(ctx context.Context, inboxID int, name, phone, identifier, avatarURL string) (*Contact, error) {
	body := map[string]any{"inbox_id": inboxID, "name": name, "identifier": identifier}
	if phone != "" {
		body["phone_number"] = phone
	}
	if avatarURL != "" {
		body["avatar_url"] = avatarURL
	}
	var out struct {
		Payload struct {
			Contact Contact `json:"contact"`
		} `json:"payload"`
	}
	if err := c.doJSON(ctx, http.MethodPost, c.accountPath("/contacts"), body, &out); err != nil {
		return nil, err
	}
	return &out.Payload.Contact, nil
}

func (c *Client) UpdateContact(ctx context.Context, contactID int, fields map[string]any) error {
	return c.doJSON(ctx, http.MethodPut, c.accountPath("/contacts/%d", contactID), fields, nil)
}

func (c *Client) ContactConversations(ctx context.Context, contactID int) ([]Conversation, error) {
	var out struct {
		Payload []Conversation `json:"payload"`
	}
	err := c.doJSON(ctx, http.MethodGet, c.accountPath("/contacts/%d/conversations", contactID), nil, &out)
	return out.Payload, err
}

// CreateConversation opens a conversation. An empty status keeps Chatwoot's default.
func (c *Client) CreateConversation(ctx context.Context, contactID, inboxID int, status string) (*Conversation, error) {
	body := map[string]any{"contact_id": contactID, "inbox_id": inboxID}
	if status != "" {
		body["status"] = status
	}
	var out Conversation
	if err := c.doJSON(ctx, http.MethodPost, c.accountPath("/conversations"), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CreateMessage(ctx context.Context, conversationID int, m NewMessage) (*CWMessage, error) {
	p := c.accountPath("/conversations/%d/messages", conversationID)
	var out CWMessage

	if m.Attachment == nil {
		body := map[string]any{"content": m.Content, "message_type": m.MessageType, "private": m.Private}
		if m.SourceID != "" {
			body["source_id"] = m.SourceID
		}
		if m.InReplyTo != 0 {
			body["content_attributes"] = map[string]any{"in_reply_to": m.InReplyTo}
		}
		if err := c.doJSON(ctx, http.MethodPost, p, body, &out); err != nil {
			return nil, err
		}
		return &out, nil
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fields := map[string]string{
		"content":      m.Content,
		"message_type": m.MessageType,
		"private":      strconv.FormatBool(m.Private),
	}
	if m.SourceID != "" {
		fields["source_id"] = m.SourceID
	}
	if m.InReplyTo != 0 {
		fields["content_attributes"] = fmt.Sprintf(`{"in_reply_to":%d}`, m.InReplyTo)
	}
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return nil, err
		}
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{
		"name":     "attachments[]",
		"filename": m.Attachment.Filename,
	}))
	contentType := m.Attachment.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	header.Set("Content-Type", contentType)
	part, err := w.CreatePart(header)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(m.Attachment.Data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	req, err := c.newRequest(ctx, http.MethodPost, p, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if err := c.send(req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateMessageStatus sets sent, delivered, read or failed. Only API inboxes accept it.
func (c *Client) UpdateMessageStatus(ctx context.Context, conversationID, messageID int, status, externalError string) error {
	body := map[string]any{"status": status}
	if externalError != "" {
		body["external_error"] = externalError
	}
	return c.doJSON(ctx, http.MethodPatch, c.accountPath("/conversations/%d/messages/%d", conversationID, messageID), body, nil)
}

func (c *Client) DeleteMessage(ctx context.Context, conversationID, messageID int) error {
	return c.doJSON(ctx, http.MethodDelete, c.accountPath("/conversations/%d/messages/%d", conversationID, messageID), nil, nil)
}

// Download fetches an attachment. Chatwoot builds attachment URLs with its
// public address; requests are sent to the configured base URL instead so the
// file is fetched over the internal network.
func (c *Client) Download(ctx context.Context, fileURL string) (*Attachment, error) {
	u, err := c.internalURL(fileURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &APIError{Method: http.MethodGet, Path: u.Path, Status: resp.StatusCode, Body: string(body)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDownloadSize {
		return nil, fmt.Errorf("attachment larger than %d bytes", maxDownloadSize)
	}

	filename := ""
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		filename = params["filename"]
	}
	if filename == "" {
		filename, _ = url.PathUnescape(path.Base(resp.Request.URL.Path))
	}
	contentType := resp.Header.Get("Content-Type")
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil {
		contentType = mediaType
	}
	return &Attachment{Filename: filename, ContentType: contentType, Data: data}, nil
}

// internalURL keeps the path and query of an attachment URL and replaces its
// scheme and host with the base URL. Only relative URLs and Rails storage paths
// are rewritten; anything else is rejected, so the client never fetches
// arbitrary addresses taken from a webhook.
func (c *Client) internalURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid attachment URL: %w", err)
	}
	cleaned := path.Clean("/" + u.Path)
	if !strings.HasPrefix(cleaned, "/rails/active_storage/") {
		return nil, fmt.Errorf("unexpected attachment URL %q", raw)
	}
	out := *c.baseURL
	out.Path = path.Join(c.baseURL.Path, cleaned)
	out.RawPath = ""
	out.RawQuery = u.RawQuery
	return &out, nil
}

// checkRedirect keeps storage redirects on the internal network and drops the
// API token when leaving the Chatwoot host (for example to S3).
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	if strings.HasPrefix(req.URL.Path, "/rails/active_storage/") {
		req.URL.Scheme = c.baseURL.Scheme
		req.URL.Host = c.baseURL.Host
		req.Host = c.baseURL.Host
	}
	if req.URL.Host != c.baseURL.Host {
		req.Header.Del("api_access_token")
	}
	return nil
}

func (c *Client) newRequest(ctx context.Context, method, p string, body io.Reader) (*http.Request, error) {
	u := *c.baseURL
	pathPart, query, _ := strings.Cut(p, "?")
	u.Path = path.Join(c.baseURL.Path, pathPart)
	u.RawQuery = query
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("api_access_token", c.token)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (c *Client) doJSON(ctx context.Context, method, p string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := c.newRequest(ctx, method, p, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.send(req, out)
}

func (c *Client) send(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Method: req.Method, Path: req.URL.Path, Status: resp.StatusCode, Body: truncate(string(data), 500)}
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("chatwoot %s %s: decode response: %w", req.Method, req.URL.Path, err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
