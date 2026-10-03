package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usageidentity"
	"strings"
	"testing"
	"time"
)

type drainPlugin struct {
	entered chan struct{}
	release chan struct{}
}

func (p *drainPlugin) HandleUsage(context.Context, Record) {
	close(p.entered)
	<-p.release
}

func TestUsagePresenceLegacyAndExplicitZero(t *testing.T) {
	if (Record{}).HasUsage() {
		t.Fatal("unflagged empty record must be missing")
	}
	if !(Record{Detail: Detail{InputTokens: 1}}).HasUsage() {
		t.Fatal("legacy nonzero record must be present")
	}
	present := true
	if !(Record{UsagePresent: &present}).HasUsage() {
		t.Fatal("explicit zero must be present")
	}
	present = false
	if (Record{UsagePresent: &present, Detail: Detail{InputTokens: 1}}).HasUsage() {
		t.Fatal("explicit missing evidence must win")
	}
}

func TestAccountIdentityOmittedFromJSON(t *testing.T) {
	record := Record{AccountIdentity: usageidentity.NewSelected("CodexExecutor", "private-provider", "private-auth", "private-workspace", "private-member", "private-org", "private-account")}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"private-provider", "private-auth", "private-workspace", "private-member", "private-org", "private-account"} {
		if strings.Contains(string(encoded), value) {
			t.Fatalf("account identity leaked into record JSON: %s", value)
		}
	}
	if (AccountIdentity{}).IsCredentialSnapshot() {
		t.Fatal("plain identity must not claim selected credential evidence")
	}
	if !record.AccountIdentity.IsCredentialSnapshot() {
		t.Fatal("selected identity seal missing")
	}
	formatted := fmt.Sprintf("%+v %#v", record, record.AccountIdentity)
	if strings.Contains(formatted, "private-workspace") || strings.Contains(formatted, "private-auth") {
		t.Fatal("formatted record leaked private identity")
	}
	selected := usageidentity.NewSelected("CodexExecutor", "provider", "auth", "workspace", "member", "", "")
	if !selected.IsCredentialSnapshot() {
		t.Fatal("selected credential constructor must mark snapshot")
	}
	encodedIdentity, err := json.Marshal(record.AccountIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if string(encodedIdentity) != "{}" {
		t.Fatalf("nested identity JSON = %s", encodedIdentity)
	}
}

func TestManagerStopAndWaitJoinsDispatch(t *testing.T) {
	manager := NewManager(1)
	plugin := &drainPlugin{entered: make(chan struct{}), release: make(chan struct{})}
	manager.Register(plugin)
	manager.Publish(context.Background(), Record{})
	<-plugin.entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.StopAndWait(ctx); err != context.Canceled {
		t.Fatalf("StopAndWait canceled context = %v", err)
	}
	close(plugin.release)
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.StopAndWait(ctx); err != nil {
		t.Fatalf("StopAndWait after dispatch release: %v", err)
	}
}
