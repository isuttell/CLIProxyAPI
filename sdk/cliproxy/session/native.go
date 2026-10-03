package session

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

var nativeIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// NativeIdentity holds client identities before affinity and routing rewrite them.
type NativeIdentity struct {
	Source                 string
	SessionID              string
	AgentID                string
	ParentSessionID        string
	OriginSessionID        string
	SessionIDInvalid       bool
	AgentIDInvalid         bool
	ParentSessionIDInvalid bool
	OriginSessionIDInvalid bool
	SessionIDAmbiguous     bool
}

// ExtractNativeIdentity only recognizes protocol-specific evidence from Claude Code or Codex.
func ExtractNativeIdentity(headers http.Header, payload []byte) NativeIdentity {
	claudeHeader := nativeHeaderValue(headers, "X-Claude-Code-Session-Id")
	claudeSession, _, claudeAgent := ClaudeMetadataIdentities(payload)
	rawClaudeSession, claudePayloadEvidence := rawClaudeMetadataSession(payload)
	claudeHeaderEvidence := nativeHeaderPresent(headers, "X-Claude-Code-Session-Id")
	if claudeHeaderEvidence || claudePayloadEvidence {
		session := claudeHeader
		if !claudeHeaderEvidence {
			session = claudeSession
			if session == "" {
				session = rawClaudeSession
			}
		}
		agent := nativeHeaderValue(headers, "X-Claude-Code-Agent-Id")
		if agent == "" {
			root := gjson.ParseBytes(payload)
			agent = root.Get("metadata.agent_id").String()
			if agent == "" {
				agent = root.Get("metadata.subagent_id").String()
			}
			if agent == "" && root.Get("request").Exists() {
				agent = root.Get("request.metadata.agent_id").String()
				if agent == "" {
					agent = root.Get("request.metadata.subagent_id").String()
				}
			}
		}
		if agent == "" {
			agent = claudeAgent
		}
		identity := NativeIdentity{Source: "claude"}
		identity.SessionID, identity.SessionIDInvalid = nativeID(session)
		identity.SessionIDInvalid = identity.SessionIDInvalid || claudeHeaderEvidence && claudeHeader == ""
		if agent != "" && !strings.EqualFold(agent, "main") {
			identity.AgentID, identity.AgentIDInvalid = nativeID(agent)
		}
		if nativeHeaderDuplicate(headers, "X-Claude-Code-Agent-Id") {
			identity.AgentID = ""
			identity.AgentIDInvalid = true
		}
		if identity.SessionID == "" {
			identity.Source = ""
			identity.AgentID = ""
		}
		return identity
	}

	turnMetadata := nativeHeaderValue(headers, "X-Codex-Turn-Metadata")
	originator := strings.ToLower(nativeHeaderValue(headers, "Originator"))
	if turnMetadata == "" && !strings.HasPrefix(originator, "codex_") {
		return NativeIdentity{}
	}
	turn := gjson.Parse(turnMetadata)
	thread := nativeHeaderValue(headers, "Thread-Id")
	if thread == "" {
		thread = nativeHeaderValue(headers, "Thread_id")
	}
	if thread == "" {
		thread = turn.Get("thread_id").String()
	}
	origin := nativeHeaderValue(headers, "Session-Id")
	if origin == "" {
		origin = nativeHeaderValue(headers, "Session_id")
	}
	if origin == "" {
		origin = turn.Get("session_id").String()
	}
	if thread == "" && origin != "" && len(payload) > 0 {
		root := gjson.ParseBytes(payload)
		thread = root.Get("thread_id").String()
		if thread == "" {
			thread = root.Get("threadId").String()
		}
		if thread == "" {
			thread = root.Get("metadata.thread_id").String()
		}
		if thread == "" && root.Get("request").Exists() {
			thread = root.Get("request.thread_id").String()
			if thread == "" {
				thread = root.Get("request.metadata.thread_id").String()
			}
		}
	}
	provenThread := thread != ""
	if thread == "" {
		thread = origin
	}
	identity := NativeIdentity{Source: "codex"}
	identity.SessionID, identity.SessionIDInvalid = nativeID(thread)
	if nativeHeaderDuplicate(headers, "Thread-Id") || nativeHeaderDuplicate(headers, "Thread_id") ||
		nativeHeaderDuplicate(headers, "Session-Id") || nativeHeaderDuplicate(headers, "Session_id") ||
		nativeHeaderPresent(headers, "Thread-Id") && nativeHeaderPresent(headers, "Thread_id") ||
		nativeHeaderPresent(headers, "Session-Id") && nativeHeaderPresent(headers, "Session_id") {
		identity.SessionID = ""
		identity.SessionIDInvalid = true
	}
	parent := nativeHeaderValue(headers, "X-Codex-Parent-Thread-Id")
	if parent == "" {
		parent = turn.Get("parent_thread_id").String()
	}
	identity.ParentSessionID, identity.ParentSessionIDInvalid = nativeID(parent)
	identity.OriginSessionID, identity.OriginSessionIDInvalid = nativeID(origin)
	subagentHeader := nativeHeaderValue(headers, "X-Openai-Subagent")
	subagentSignal := subagentHeader != "" && !strings.EqualFold(subagentHeader, "false") && subagentHeader != "0"
	if turn.Get("subagent_kind").String() == "thread_spawn" {
		subagentSignal = true
	}
	if name := strings.TrimPrefix(turn.Get("agent_name").String(), "/root/"); name != "" && name != "root" && name != "main" {
		subagentSignal = true
	}
	if identity.SessionID != "" && (identity.SessionID == identity.ParentSessionID || !provenThread && subagentSignal) {
		identity.SessionIDAmbiguous = true
		identity.SessionID = ""
		identity.ParentSessionID = ""
		identity.OriginSessionID = ""
	}
	if identity.SessionID == "" {
		identity.Source = ""
		identity.ParentSessionID = ""
		identity.OriginSessionID = ""
	} else if identity.SessionID == identity.OriginSessionID {
		identity.OriginSessionID = ""
	}
	return identity
}

func nativeHeaderValue(headers http.Header, name string) string {
	value := ""
	count := 0
	for key, values := range headers {
		if strings.EqualFold(key, name) {
			count += len(values)
			if len(values) == 1 {
				value = strings.TrimSpace(values[0])
			}
		}
	}
	if count != 1 {
		return ""
	}
	return value
}

func nativeHeaderDuplicate(headers http.Header, name string) bool {
	count := 0
	for key, values := range headers {
		if strings.EqualFold(key, name) {
			count += len(values)
		}
	}
	return count > 1
}

func nativeHeaderPresent(headers http.Header, name string) bool {
	for key := range headers {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

func nativeID(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	value := normalizedSessionCandidate(raw)
	if !nativeIDPattern.MatchString(value) {
		return "", true
	}
	return value, false
}

func rawClaudeMetadataSession(payload []byte) (string, bool) {
	if len(payload) == 0 {
		return "", false
	}
	root := gjson.ParseBytes(payload)
	userID := root.Get("metadata.user_id").String()
	if userID == "" && root.Get("request").Exists() {
		userID = root.Get("request.metadata.user_id").String()
	}
	if strings.HasPrefix(strings.TrimSpace(userID), "{") {
		parsed := gjson.Parse(userID)
		if session := parsed.Get("session_id"); session.Exists() {
			return session.String(), true
		}
	}
	if match := legacyClaudeSessionPattern.FindStringSubmatch(userID); len(match) >= 2 {
		return match[1], true
	}
	return "", false
}
