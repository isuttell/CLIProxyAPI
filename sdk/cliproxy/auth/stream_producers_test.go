package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestSealAndWaitStreamProducersJoinsDetachedDrain(t *testing.T) {
	manager := &Manager{}
	chunks := make(chan cliproxyexecutor.StreamChunk)
	manager.discardStreamChunks(chunks, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.SealAndWaitStreamProducers(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait while drain active = %v", err)
	}
	close(chunks)
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.SealAndWaitStreamProducers(ctx); err != nil {
		t.Fatalf("wait after drain: %v", err)
	}
}

func TestStreamProducerChildKeepsFenceOpen(t *testing.T) {
	manager := &Manager{}
	tracker := &manager.streamProducers
	if !tracker.begin() {
		t.Fatal("first producer rejected")
	}
	tracker.fork()
	tracker.end()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.SealAndWaitStreamProducers(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("child should keep fence open: %v", err)
	}
	manager.streamProducers.end()
	if err := manager.SealAndWaitStreamProducers(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSealedStreamReturnsErrorAndDrainsProducer(t *testing.T) {
	manager := &Manager{}
	if err := manager.SealAndWaitStreamProducers(context.Background()); err != nil {
		t.Fatal(err)
	}
	upstream := make(chan cliproxyexecutor.StreamChunk)
	result := manager.wrapStreamResult(context.Background(), nil, "codex", "model", "model", nil, nil, upstream, OAuthModelAliasResult{}, false, cliproxyexecutor.Options{})
	chunk, ok := <-result.Chunks
	if !ok || !errors.Is(chunk.Err, context.Canceled) {
		t.Fatalf("sealed stream result = %+v, %v", chunk, ok)
	}
	sent := make(chan struct{})
	go func() {
		upstream <- cliproxyexecutor.StreamChunk{Payload: []byte("late")}
		close(sent)
		close(upstream)
	}()
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("sealed stream abandoned upstream producer")
	}
	if _, ok := <-result.Chunks; ok {
		t.Fatal("sealed stream emitted extra chunk")
	}
}
