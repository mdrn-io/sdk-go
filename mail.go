package mdrn

import (
	"context"
	"net/http"
)

// Mail is the Mail product: one message at a time.
type Mail struct {
	c *Client
}

// Attachment is a file sent with a message: at most four per message, 256 KiB decoded in all;
// Type is application/pdf, image/png, image/jpeg or text/plain; Content is base64.
type Attachment struct {
	Filename string `json:"filename"`
	Type     string `json:"type"`
	Content  string `json:"content"`
}

// Message is a message to send: either your own content (Subject and Text, HTML if wanted) or
// a Template of the project and environment, which supplies all three. Attachments and
// SenderID (a verified sender domain; leave it empty when the workspace has one) go with
// either.
type Message struct {
	To          string
	Subject     string
	Text        string
	HTML        string
	Attachments []Attachment
	Template    string
	SenderID    string
}

// Sent is an accepted message: queued for delivery, not yet delivered.
type Sent struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	RequestID string `json:"requestId"`
}

// Only the fields given are sent, in this order.
type messageInput struct {
	Recipient   string       `json:"recipient"`
	Subject     string       `json:"subject,omitempty"`
	Text        string       `json:"text,omitempty"`
	HTML        string       `json:"html,omitempty"`
	TemplateID  string       `json:"templateId,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	SenderID    string       `json:"senderId,omitempty"`
}

// Send queues the message. A template together with a subject, text or HTML is refused with
// an ArgumentError before anything is sent. Options: Key.
func (m *Mail) Send(ctx context.Context, message Message, opts ...Option) (*Sent, error) {
	o, err := options(opts)
	if err != nil {
		return nil, err
	}
	if message.To == "" {
		return nil, argument("to is required")
	}
	if message.Template != "" && (message.Subject != "" || message.Text != "" || message.HTML != "") {
		return nil, argument("a template supplies subject, text and html; send either a template or your own content")
	}
	body := messageInput{Recipient: message.To, Subject: message.Subject, Text: message.Text, HTML: message.HTML, TemplateID: message.Template, Attachments: message.Attachments, SenderID: message.SenderID}
	var sent Sent
	if err := m.c.call(ctx, m.c.mail, http.MethodPost, "/messages", body, o.keyOrNew(), 0, &sent); err != nil {
		return nil, err
	}
	return &sent, nil
}
