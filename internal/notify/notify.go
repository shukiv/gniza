// Package notify sends word of what a backup did to wherever the operator
// is looking.
//
// A backup that fails silently is the same as no backup at all. This
// program can already say what happened on its own pages; nobody reads a
// page for a server that is working, which is exactly when it stops.
package notify

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Kind is a way of reaching someone.
type Kind string

const (
	KindSMTP     Kind = "smtp"
	KindNtfy     Kind = "ntfy"
	KindTelegram Kind = "telegram"
	KindWebhook  Kind = "webhook"
)

// Kinds is every kind, in the order the interface offers them.
var Kinds = []Kind{KindSMTP, KindNtfy, KindTelegram, KindWebhook}

// Title is the kind as an operator would name it.
func (k Kind) Title() string {
	switch k {
	case KindSMTP:
		return "Email"
	case KindNtfy:
		return "ntfy"
	case KindTelegram:
		return "Telegram"
	case KindWebhook:
		return "Webhook"
	}
	return string(k)
}

// Event is something worth telling someone about.
type Event string

const (
	// EventBackupFailed is a backup that did not work.
	EventBackupFailed Event = "backup_failed"
	// EventBackupPartial is one that worked with files it could not read.
	EventBackupPartial Event = "backup_partial"
	// EventBackupSucceeded is every backup, for whoever wants to see them.
	EventBackupSucceeded Event = "backup_ok"
	// EventOverdue is an account whose backups have stopped happening,
	// which nothing else here would ever raise: no run means no failure.
	EventOverdue Event = "overdue"
	// EventStuck is a run that has been going far longer than it should.
	EventStuck Event = "stuck"
	// EventDestinationDown is a destination that could not be reached.
	EventDestinationDown Event = "destination_down"
	// EventCheckFailed is a repository that restic found damaged. It is
	// the one event here about backups already taken rather than the one
	// being taken.
	EventCheckFailed Event = "check_failed"
	// EventRestore is a restore finishing, either way.
	EventRestore Event = "restore"
	// EventStarted is a backup or a restore beginning. Off unless
	// somebody asks for it: on a server with a nightly schedule it is one
	// message per account per night, which is a great deal of mail for
	// news that nothing has gone wrong. It is here for the operator who
	// wants to know the moment a customer's restore begins.
	EventStarted Event = "started"
)

// Events is every event, in the order the interface offers them.
var Events = []Event{
	EventBackupFailed, EventBackupPartial, EventOverdue, EventStuck,
	EventDestinationDown, EventCheckFailed, EventRestore, EventBackupSucceeded,
	EventStarted,
}

// Title is the event as an operator would name it.
func (e Event) Title() string {
	switch e {
	case EventBackupFailed:
		return "A backup failed"
	case EventBackupPartial:
		return "A backup could not read every file"
	case EventBackupSucceeded:
		return "Every backup, including the ones that worked"
	case EventOverdue:
		return "An account has stopped being backed up"
	case EventStuck:
		return "A run is taking far too long"
	case EventDestinationDown:
		return "A destination could not be reached"
	case EventCheckFailed:
		return "A repository failed its integrity check"
	case EventRestore:
		return "A restore finished"
	case EventStarted:
		return "A backup or restore started"
	}
	return string(e)
}

// Severity decides how loudly a channel should say it.
type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

// Severity of an event, for the channels that have a notion of it.
func (e Event) Severity() Severity {
	switch e {
	case EventBackupFailed, EventStuck, EventDestinationDown, EventOverdue,
		EventCheckFailed:
		return SeverityError
	case EventBackupPartial:
		return SeverityWarning
	default:
		return SeverityInfo
	}
}

// Message is what gets sent.
type Message struct {
	Event Event
	// Subject is one line: what happened, to what.
	Subject string
	// Body says the rest, in whole sentences. A notification that says
	// "job failed" and nothing else costs someone a login to find out
	// what it means.
	Body string
	// Host is the server it happened on, which matters the moment an
	// operator runs more than one.
	Host string
	// Account is what it happened to, empty for anything server-wide.
	Account string
	// Level overrides the event's own severity, for the one case where
	// the event and the news disagree: a destination coming back is the
	// destination_down event, and it is not an alarm.
	Level Severity
}

// Severity of the message, from its event unless it said otherwise.
func (m Message) Severity() Severity {
	if m.Level != "" {
		return m.Level
	}
	return m.Event.Severity()
}

// Line is the subject with the server in front of it, which is what a
// chat message or a phone notification shows.
func (m Message) Line() string {
	if m.Host == "" {
		return m.Subject
	}
	return m.Host + ": " + m.Subject
}

// Text is the whole message as plain text.
func (m Message) Text() string {
	var out strings.Builder
	out.WriteString(m.Line())
	if m.Body != "" {
		out.WriteString("\n\n")
		out.WriteString(m.Body)
	}
	return out.String()
}

// Channel is somewhere messages are sent.
type Channel struct {
	Kind Kind
	Name string
	// Config is the channel's settings: a server address, a topic, a
	// chat id. Nothing secret.
	Config map[string]string
	// Secrets are the credentials, kept apart because they are sealed in
	// the vault and never rendered back into a page.
	Secrets map[string]string
	// Events it wants. Empty means the ones that report a problem.
	Events []Event
}

// Wants reports whether this channel asked about an event.
func (c Channel) Wants(event Event) bool {
	if len(c.Events) == 0 {
		return event.Severity() != SeverityInfo
	}
	for _, wanted := range c.Events {
		if wanted == event {
			return true
		}
	}
	return false
}

// Sender delivers a message over one kind of channel.
type Sender interface {
	Send(ctx context.Context, channel Channel, message Message) error
}

// Senders is a sender per kind.
var Senders = map[Kind]Sender{
	KindSMTP:     &SMTP{},
	KindNtfy:     &Ntfy{},
	KindTelegram: &Telegram{},
	KindWebhook:  &Webhook{},
}

// Send delivers one message over one channel.
func Send(ctx context.Context, channel Channel, message Message) error {
	sender, known := Senders[channel.Kind]
	if !known {
		return fmt.Errorf("notify: %q is not a way of sending anything", channel.Kind)
	}
	return sender.Send(ctx, channel, message)
}

// Required is what a kind cannot be configured without, so the interface
// can say which field is missing rather than failing when something
// actually goes wrong.
func Required(kind Kind) (config, secrets []string) {
	switch kind {
	case KindSMTP:
		return []string{"host", "from", "to"}, nil
	case KindNtfy:
		return []string{"topic"}, nil
	case KindTelegram:
		return []string{"chat_id"}, []string{"token"}
	case KindWebhook:
		return []string{"url"}, nil
	}
	return nil, nil
}

// Missing names the settings a channel has not been given.
func Missing(channel Channel) []string {
	config, secrets := Required(channel.Kind)
	var missing []string
	for _, key := range config {
		if strings.TrimSpace(channel.Config[key]) == "" {
			missing = append(missing, key)
		}
	}
	for _, key := range secrets {
		if strings.TrimSpace(channel.Secrets[key]) == "" {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	return missing
}
