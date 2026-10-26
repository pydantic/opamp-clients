//go:build container

// The container image, end to end: run it against a server that issues a token, and check that
// a new container on the same volume connects with the issued token, kept sealed in the volume.
// It needs Docker and the image (container/build.sh), named by OPAMP_IMAGE:
//
//	OPAMP_IMAGE=pydantic-opamp-supervisor:0.0.0 go test -tags=container -run Container .

package packaging

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	require.NoError(t, err, "docker %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// connected waits for check to pass, and shows the container's logs if it doesn't.
func connected(t *testing.T, ctr string, check func(c *assert.CollectT)) {
	t.Helper()
	if !assert.EventuallyWithT(t, check, 60*time.Second, 200*time.Millisecond) {
		out, _ := exec.Command("docker", "logs", ctr).CombinedOutput()
		t.Fatalf("the container's logs:\n%s", out)
	}
}

func TestContainerEnrollRestart(t *testing.T) {
	image := os.Getenv("OPAMP_IMAGE")
	if image == "" {
		t.Skip("OPAMP_IMAGE names the image to test")
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	vol := "opamp-test-" + hex.EncodeToString(b)
	docker(t, "volume", "create", vol)
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", vol).Run() })

	srv := newIssuingServer(t)
	start := func() string {
		// The host's network: the server listens on the host's loopback.
		ctr := docker(t, "run", "-d", "--network", "host",
			"-e", "OPAMP_ENDPOINT="+srv.endpoint, "-e", "OPAMP_TOKEN=enroll-token",
			"-e", "OPAMP_STATE_KEY=file:/var/lib/pydantic-opamp/state.key",
			"-v", vol+":/var/lib/pydantic-opamp", image)
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", ctr).Run() })
		return ctr
	}

	ctr := start()
	connected(t, ctr, func(c *assert.CollectT) {
		auths, statuses := srv.seen()
		assert.Contains(c, auths, "Bearer issued-token")
		assert.Contains(c, statuses, protobufs.ConnectionSettingsStatuses_ConnectionSettingsStatuses_APPLIED)
	})
	// `status` in the running container says it's connected, to the endpoint configured.
	connected(t, ctr, func(c *assert.CollectT) {
		out, _ := exec.Command("docker", "exec", ctr, "/usr/lib/pydantic-opamp/opampsupervisor", "status",
			"--config", "/etc/pydantic-opamp/supervisor.yaml").CombinedOutput()
		assert.Contains(c, string(out), srv.endpoint+": connected since")
	})
	docker(t, "stop", ctr)

	dir := t.TempDir()
	docker(t, "cp", ctr+":/var/lib/pydantic-opamp/opamp-state", filepath.Join(dir, "opamp-state"))
	state, err := os.ReadFile(filepath.Join(dir, "opamp-state"))
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(state, []byte("OPAMPSK1")), "the state file is sealed")
	assert.NotContains(t, string(state), "issued-token")

	// A new container on the volume is the same agent, with the token it was issued.
	srv.forget()
	ctr = start()
	connected(t, ctr, func(c *assert.CollectT) {
		auths, _ := srv.seen()
		if assert.NotEmpty(c, auths) {
			assert.Equal(c, "Bearer issued-token", auths[0], "the new container connects with the issued token")
		}
	})
	auths, _ := srv.seen()
	assert.NotContains(t, auths, "Bearer enroll-token")
}
