package session

import (
	"net/http"
	"strings"
	"testing"
)

func TestExtractNativeIdentity(t *testing.T) {
	tests := []struct {
		name    string
		headers http.Header
		body    string
		want    NativeIdentity
	}{
		{"claude header", http.Header{"X-Claude-Code-Session-Id": {"claude-session"}, "X-Claude-Code-Agent-Id": {"agent-1"}}, "", NativeIdentity{Source: "claude", SessionID: "claude-session", AgentID: "agent-1"}},
		{"claude metadata", nil, `{"metadata":{"user_id":"{\"session_id\":\"claude-body\",\"agent_id\":\"agent-2\"}"}}`, NativeIdentity{Source: "claude", SessionID: "claude-body", AgentID: "agent-2"}},
		{"claude legacy", nil, `{"metadata":{"user_id":"user_session_abc123"}}`, NativeIdentity{Source: "claude", SessionID: "abc123"}},
		{"claude main agent omitted", http.Header{"X-Claude-Code-Session-Id": {"claude-session"}, "X-Claude-Code-Agent-Id": {"main"}}, "", NativeIdentity{Source: "claude", SessionID: "claude-session"}},
		{"claude wins", http.Header{"X-Claude-Code-Session-Id": {"claude-session"}, "Session-Id": {"codex-origin"}, "X-Codex-Turn-Metadata": {`{"thread_id":"codex-thread"}`}}, "", NativeIdentity{Source: "claude", SessionID: "claude-session"}},
		{"codex thread and origin", http.Header{"Thread-Id": {"child-thread"}, "Session-Id": {"root-session"}, "X-Codex-Turn-Metadata": {`{"parent_thread_id":"root-thread","agent_name":"reviewer"}`}}, "", NativeIdentity{Source: "codex", SessionID: "child-thread", ParentSessionID: "root-thread", OriginSessionID: "root-session"}},
		{"codex underscore aliases", http.Header{"Thread_id": {"child-thread"}, "Session_id": {"root-session"}, "Originator": {"codex_cli"}}, "", NativeIdentity{Source: "codex", SessionID: "child-thread", OriginSessionID: "root-session"}},
		{"codex body thread", http.Header{"Session-Id": {"root-session"}, "Originator": {"codex_cli"}}, `{"metadata":{"thread_id":"child-thread"}}`, NativeIdentity{Source: "codex", SessionID: "child-thread", OriginSessionID: "root-session"}},
		{"codex root path without thread", http.Header{"Session-Id": {"root-session"}, "X-Codex-Turn-Metadata": {`{"agent_name":"/root"}`}}, "", NativeIdentity{Source: "codex", SessionID: "root-session"}},
		{"codex child path without thread ambiguous", http.Header{"Session-Id": {"root-session"}, "X-Codex-Turn-Metadata": {`{"agent_name":"/root/reviewer"}`}}, "", NativeIdentity{SessionIDAmbiguous: true}},
		{"codex no thread root alias ambiguous", http.Header{"Session-Id": {"root-session"}, "X-Openai-Subagent": {"true"}, "Originator": {"codex_cli"}}, "", NativeIdentity{SessionIDAmbiguous: true}},
		{"codex self parent ambiguous", http.Header{"Session-Id": {"root-session"}, "X-Codex-Turn-Metadata": {`{"parent_thread_id":"root-session"}`}}, "", NativeIdentity{SessionIDAmbiguous: true}},
		{"codex role is not agent id", http.Header{"Thread-Id": {"child-thread"}, "X-Codex-Turn-Metadata": {`{"agent_name":"/root/reviewer"}`}}, "", NativeIdentity{Source: "codex", SessionID: "child-thread"}},
		{"bare session", http.Header{"Session-Id": {"generic-session"}}, "", NativeIdentity{}},
		{"generic routing", http.Header{"X-Session-ID": {"generic-session"}}, "", NativeIdentity{}},
		{"invalid claude does not become codex", http.Header{"X-Claude-Code-Session-Id": {strings.Repeat("a", 129)}, "Session-Id": {"codex-session"}, "Originator": {"codex_cli"}}, "", NativeIdentity{SessionIDInvalid: true}},
		{"duplicate claude header", http.Header{"X-Claude-Code-Session-Id": {"first", "second"}}, "", NativeIdentity{SessionIDInvalid: true}},
		{"duplicate codex thread header", http.Header{"Thread-Id": {"child-a", "child-b"}, "Session-Id": {"root"}, "Originator": {"codex_cli"}}, "", NativeIdentity{SessionIDInvalid: true}},
		{"conflicting codex thread aliases", http.Header{"Thread-Id": {"child-a"}, "Thread_id": {"child-b"}, "Originator": {"codex_cli"}}, "", NativeIdentity{SessionIDInvalid: true}},
		{"duplicate claude agent header", http.Header{"X-Claude-Code-Session-Id": {"claude-session"}, "X-Claude-Code-Agent-Id": {"agent-a", "agent-b"}}, "", NativeIdentity{Source: "claude", SessionID: "claude-session", AgentIDInvalid: true}},
		{"invalid claude metadata does not become codex", http.Header{"Session-Id": {"codex-session"}, "Originator": {"codex_cli"}}, `{"metadata":{"user_id":"{\"session_id\":\"bad@example.com\"}"}}`, NativeIdentity{SessionIDInvalid: true}},
		{"invalid agent", http.Header{"X-Claude-Code-Session-Id": {"claude-session"}, "X-Claude-Code-Agent-Id": {"bad@example.com"}}, "", NativeIdentity{Source: "claude", SessionID: "claude-session", AgentIDInvalid: true}},
		{"control character", http.Header{"X-Claude-Code-Session-Id": {"bad\nvalue"}}, "", NativeIdentity{SessionIDInvalid: true}},
		{"oversized session", http.Header{"X-Claude-Code-Session-Id": {strings.Repeat("a", 257)}}, "", NativeIdentity{SessionIDInvalid: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ExtractNativeIdentity(test.headers, []byte(test.body))
			if got != test.want {
				t.Fatalf("native identity = %+v, want %+v", got, test.want)
			}
		})
	}
}
