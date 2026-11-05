// Package core contains the portable helper model and application contracts.
// It uses no process execution, syscalls or platform path conventions. Protocol
// timestamps are Unix seconds, including fractions, as in the Python hub.
package core

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/llcoolkm/rookery/go/platform"
)

const (
	Version       = "0.3.1"
	MaxBodyBytes  = 16384
	MaxHops       = 6
	RequestWindow = 7 * 24 * time.Hour
	ClockSkew     = 5 * time.Minute
)

type Kind string

const (
	Ask    Kind = "ask"
	Inform Kind = "inform"
	Reply  Kind = "reply"
)

type Status string

const (
	Pending      Status = "pending"
	Read         Status = "read"
	Dispatching  Status = "dispatching"
	Submitted    Status = "submitted"
	Uncertain    Status = "uncertain"
	Failed       Status = "failed"
	Acknowledged Status = "acknowledged"
	Replied      Status = "replied"
	Expired      Status = "expired"
	Cancelled    Status = "cancelled"
)

// Message is the hub snapshot, not proof of model receipt. Optional booleans
// and timestamps use pointers so false and zero survive JSON serialization.
type Message struct {
	Extra            map[string]json.RawMessage `json:"-"`
	V                int                        `json:"v"`
	TTL              int                        `json:"ttl"`
	RequestedExpires *float64                   `json:"requested_expires"`
	Updated          float64                    `json:"updated"`
	ReceivedAt       *float64                   `json:"received_at,omitempty"`
	BodyRetained     *bool                      `json:"body_retained,omitempty"`
	ID               string                     `json:"id"`
	Sender           string                     `json:"sender"`
	Recipient        string                     `json:"recipient"`
	Kind             Kind                       `json:"kind"`
	Body             string                     `json:"body"`
	Status           Status                     `json:"status"`
	Created          float64                    `json:"created"`
	Expires          float64                    `json:"expires"`
	ReceivedByHost   *bool                      `json:"received_by_host,omitempty"`
	ReadAt           *float64                   `json:"read_at,omitempty"`
	Detail           *string                    `json:"detail,omitempty"`
	ReplyID          string                     `json:"reply_id,omitempty"`
	InReplyTo        *string                    `json:"in_reply_to"`
	FollowupID       string                     `json:"followup_id,omitempty"`
	Conversation     string                     `json:"conversation,omitempty"`
	Hops             int                        `json:"hops"`
	ClaimID          string                     `json:"claim_id,omitempty"`
}

// Envelope is the native-delivery JSON line. Conversation is omitted when it
// equals InReplyTo; rendering must escape all Unicode line separators.
type Envelope struct {
	Extra        map[string]json.RawMessage `json:"-"`
	ID           string                     `json:"id"`
	Sender       string                     `json:"sender"`
	Kind         Kind                       `json:"kind"`
	Body         string                     `json:"body"`
	InReplyTo    string                     `json:"in_reply_to,omitempty"`
	Conversation string                     `json:"conversation,omitempty"`
}

type RPCRequest struct {
	Extra map[string]json.RawMessage `json:"-"`
	Op    string                     `json:"op"`
	Data  json.RawMessage            `json:"data"`
}
type RPCResponse struct {
	Extra     map[string]json.RawMessage `json:"-"`
	OK        bool                       `json:"ok"`
	Result    json.RawMessage            `json:"result,omitempty"`
	Error     string                     `json:"error,omitempty"`
	Code      string                     `json:"code,omitempty"`
	Retryable bool                       `json:"retryable,omitempty"`
}

// Command is a durable local request. Its identity survives all retries.
type Command struct {
	Extra   map[string]json.RawMessage `json:"-"`
	ID      string                     `json:"id"`
	Op      string                     `json:"op"`
	Data    json.RawMessage            `json:"data"`
	Created float64                    `json:"created"`
}
type Event struct {
	Extra map[string]json.RawMessage `json:"-"`
	Seq   int64                      `json:"seq"`
	Type  string                     `json:"type"`
	Data  json.RawMessage            `json:"data"`
}
type Session struct {
	// ProcessRef is captured at discovery and never sent over the hub wire.
	ProcessRef        *platform.ProcessRef       `json:"-"`
	Extra             map[string]json.RawMessage `json:"-"`
	Node              string                     `json:"node,omitempty"`
	Online            *bool                      `json:"online,omitempty"`
	EnrollmentCurrent *bool                      `json:"enrollment_current,omitempty"`
	DeliveryBlocker   string                     `json:"delivery_blocker,omitempty"`
	ID                string                     `json:"id"`
	Provider          string                     `json:"provider"`
	Key               string                     `json:"key,omitempty"`
	Name              string                     `json:"name"`
	Status            string                     `json:"status"`
	PID               int                        `json:"pid,omitempty"`
	Delivery          string                     `json:"delivery,omitempty"`
	Admission         string                     `json:"admission,omitempty"`
	Enrolled          bool                       `json:"enrolled"`
	Alias             string                     `json:"alias,omitempty"`
}

// HubClient calls the existing HTTPS /v2/rpc endpoint. A definite rejection
// differs from an unknown transport outcome; neither may create a fresh ID.
// Implementations bound responses, reject redirects, and redact credentials.
type HubClient interface {
	Call(context.Context, RPCRequest) (json.RawMessage, error)
}

// Store owns durable local state. Apply commits snapshots and cursor together.
// StartAttempt MUST persist before native I/O. Recovery changes unfinished
// attempts to uncertain, and never retries their native submission.
type Store interface {
	Admit(context.Context, Command) error
	Commands(context.Context, int) ([]Command, error)
	Complete(context.Context, string, RPCResponse) error
	Apply(context.Context, []Event, int64) error
	Cursor(context.Context) (int64, error)
	Messages(context.Context, string, int) ([]Message, error)
	PendingReads(context.Context) (map[string][]string, error)
	StartAttempt(context.Context, string, string) error
	FinishAttempt(context.Context, string, DeliveryResult) error
	RecoverAttempts(context.Context) error
	Reports(context.Context) ([]Attempt, error)
	MarkReported(context.Context, string) error
	Prune(context.Context, time.Time) error
}

type Attempt struct {
	ClaimID, MessageID, RequestID string
	Result                        DeliveryResult
}

// DeliveryResult is held, submitted, failed or uncertain. Only held guarantees
// no native I/O occurred; submitted is never an acknowledgment by the model.
type DeliveryResult struct {
	// LocalReason is a fixed diagnostic code, excluded from persistence and RPC.
	LocalReason string                     `json:"-"`
	Extra       map[string]json.RawMessage `json:"-"`
	Status      string                     `json:"status"`
	Detail      string                     `json:"detail,omitempty"`
}

// Enrollment binds consent and aliases to an exact session, never a reused PID.
type Enrollment interface {
	Enroll(context.Context, string, string) (Session, error)
	Disable(context.Context, string) error
	ClearAlias(context.Context, string) error
}

// DeliveryMachine claims at the hub, journals locally, submits once, and reports.
// A pending durable read suppresses claims until the hub confirms consumption.
type DeliveryMachine interface {
	Deliver(context.Context, Message) (DeliveryResult, error)
	Recover(context.Context) error
	Report(context.Context) error
}

// CallMetadata is harness-injected per-call metadata, never tool arguments.
type CallMetadata map[string]json.RawMessage

// MCPServer serves bounded newline JSON-RPC with the 0.3.1 compact tool results.
// Caller identity is resolved on each call and missing identity fails closed.
type MCPServer interface {
	Serve(context.Context, io.Reader, io.Writer) error
}
