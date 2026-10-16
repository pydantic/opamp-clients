package opampotel

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// The client's saved state (opamp-state) holding a config applied, as the client leaves it once
// the server has heard APPLIED: what the next start's settings come from. A test without a client
// writes it here. The bytes are a state the client saved before it kept the config applied (the
// shared vectors' start.json), with the config applied added: its field 16, a message, which
// protobuf takes from the end of the encoding. The config holds doc as its only policy file.
func coreState(t testing.TB, doc string) []byte {
	t.Helper()
	start := readVectors[struct {
		Cases []struct {
			Name      string `json:"name"`
			CoreState string `json:"core_state"`
		} `json:"cases"`
	}](t, "start.json")
	const before = "a core state saved before the core kept the config applied: the policy kept before carries over"
	for _, c := range start.Cases {
		if c.Name != before {
			continue
		}
		base, err := hex.DecodeString(c.CoreState)
		if err != nil {
			t.Fatal(err)
		}
		applied, err := proto.Marshal(&protobufs.AgentRemoteConfig{ConfigHash: []byte("applied"), Config: &protobufs.AgentConfigMap{
			ConfigMap: map[string]*protobufs.AgentConfigObject{policyFile: {Body: []byte(doc), ContentType: "application/json"}}}})
		if err != nil {
			t.Fatal(err)
		}
		return protowire.AppendBytes(protowire.AppendTag(base, 16, protowire.BytesType), applied)
	}
	t.Fatalf("start.json has no case %q", before)
	return nil
}

// keepApplied leaves in dir what the client would after a config with the policy doc was applied.
func keepApplied(t testing.TB, dir, doc string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "opamp-state"), coreState(t, doc), 0o600); err != nil {
		t.Fatal(err)
	}
}
