package agent

import (
	"encoding/json"
	"testing"
)

func TestSeedPriorTurnsTextMatchesNodeMessageTags(t *testing.T) {
	raw := json.RawMessage(`[{"role":"user","name":"A&B","text":"hello <there>"},{"role":"assistant","name":"QM","text":"answer"},{"role":"user","text":" hello   <there> "}]`)
	got := seedPriorTurnsText(raw)
	want := `<message from="human" author="A&amp;B">hello &lt;there&gt;</message>` + "\n" + `<message from="agent" via="QM">answer</message>`
	if got != want {
		t.Fatalf("got=%q want=%q", got, want)
	}
}
