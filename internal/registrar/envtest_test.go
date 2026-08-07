package registrar

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	coreV1 "k8s.io/api/core/v1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// startEnvtest brings up a real apiserver, or skips.
//
// No build tag. A tagged test that CI never runs rots quietly and gets deleted
// six months later; this one at least compiles on every `go build`. The cost is
// that a skip is indistinguishable from a pass, so CI asserts the case actually
// RAN rather than trusting the exit code.
func startEnvtest(t *testing.T) *rest.Config {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is unset; run `setup-envtest use` to enable this")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Logf("stop envtest: %v", err)
		}
	})
	return cfg
}

// A registration whose source namespace vanished while the process was down is
// collected on startup.
//
// This is the one thing neither the kind job nor the managerOptions assertions
// cover. Registration and deletion are driven by the namespace watch, but a
// namespace deleted while nothing was running produces no event to catch up on:
// the only thing that finds it is the startup seeder, which reconstructs the key
// set from the source-namespace label of every owned Secret. Delete that
// runnable and everything else still works, while an orphaned registration
// survives forever with no symptom at all.
func TestAStrandedRegistrationIsCollectedOnStartup(t *testing.T) {
	base := startEnvtest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := ClientFor(base)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if _, err := client.CoreV1().Namespaces().Create(ctx,
		&coreV1.Namespace{ObjectMeta: metaV1.ObjectMeta{Name: testTargetNS}},
		metaV1.CreateOptions{}); err != nil {
		t.Fatalf("create %s: %v", testTargetNS, err)
	}

	// Records a source namespace that has NEVER existed. If it existed at any
	// point while the manager ran, the watch would deliver its deletion and the
	// seeder would not be what collected this.
	orphan := registeredSecret("ghost", "k3k-ghost")
	if _, err := client.CoreV1().Secrets(testTargetNS).Create(ctx, orphan, metaV1.CreateOptions{}); err != nil {
		t.Fatalf("create orphan: %v", err)
	}

	r := NewWithClient(slog.New(slog.NewTextHandler(io.Discard, nil)), testConfig(), client)

	// Buffered, and selected on below: Start blocks, so an immediate failure would
	// otherwise be indistinguishable from a slow start until the deadline expires
	// and reports the wrong thing.
	errCh := make(chan error, 1)
	go func() {
		errCh <- r.Start(ctx, ControllerOptions{
			Interval:   time.Second,
			RestConfig: base,
			// Empty, so the test binds no ports and parallel runs cannot collide.
			HealthProbeBindAddress: "",
		})
	}()

	deadline := time.After(60 * time.Second)
	for {
		select {
		case err := <-errCh:
			t.Fatalf("manager exited before collecting the orphan: %v", err)
		case <-deadline:
			t.Fatal("the stranded registration was never collected; the startup seeder " +
				"is the only thing that would have found it")
		case <-time.After(time.Second):
			_, err := client.CoreV1().Secrets(testTargetNS).
				Get(ctx, orphan.Name, metaV1.GetOptions{})
			if apiErrors.IsNotFound(err) {
				return
			}
			if err != nil {
				t.Fatalf("get orphan: %v", err)
			}
		}
	}
}
