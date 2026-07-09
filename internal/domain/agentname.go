package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// Agent short names: pane ids like "w6:p1" are not operator-friendly, so
// each agent gets a generated adjective-animal name (e.g. "brave-otter").
// Generation is deterministic from the agent id so the same pane tends to
// get the same name; uniqueness is guaranteed by probing against the
// caller's taken-set (persistence lives in the store).

var nameAdjectives = []string{
	"agile", "amber", "bold", "brave", "bright", "brisk", "calm", "clever",
	"cosmic", "crimson", "daring", "deft", "eager", "fleet", "gentle", "golden",
	"happy", "hardy", "honest", "humble", "jade", "keen", "lively", "lucky",
	"mellow", "mighty", "nimble", "noble", "patient", "plucky", "proud", "quick",
	"quiet", "rapid", "rosy", "sage", "sharp", "silver", "steady", "sunny",
	"swift", "tidy", "trusty", "vivid", "wise", "witty", "zesty",
}

var nameAnimals = []string{
	"badger", "bison", "crane", "dingo", "dolphin", "falcon", "ferret", "finch",
	"fox", "gecko", "heron", "hound", "ibex", "jackal", "koala", "lemur",
	"llama", "lynx", "magpie", "marmot", "marten", "mole", "moose", "narwhal",
	"ocelot", "orca", "osprey", "otter", "owl", "panda", "pelican", "pika",
	"puffin", "quokka", "rabbit", "raven", "robin", "seal", "shrew", "sparrow",
	"stoat", "swift", "tapir", "toucan", "walrus", "wombat", "wren", "yak",
}

// GenerateAgentName returns a short friendly name for agentID that is not
// already taken. It hashes the id for a stable starting point, then probes
// forward; after exhausting adjective-animal combinations it falls back to
// numbered suffixes, so it always terminates with a unique name.
func GenerateAgentName(agentID string, taken func(string) bool) string {
	sum := sha256.Sum256([]byte("agent-name|" + agentID))
	seed := binary.BigEndian.Uint64(sum[:8])
	total := uint64(len(nameAdjectives) * len(nameAnimals))

	for i := uint64(0); i < total; i++ {
		idx := (seed + i) % total
		name := nameAdjectives[idx%uint64(len(nameAdjectives))] + "-" +
			nameAnimals[idx/uint64(len(nameAdjectives))]
		if !taken(name) {
			return name
		}
	}
	// Every combination taken (herds this size deserve numbered names).
	for n := 2; ; n++ {
		idx := seed % total
		name := fmt.Sprintf("%s-%s-%d",
			nameAdjectives[idx%uint64(len(nameAdjectives))],
			nameAnimals[idx/uint64(len(nameAdjectives))], n)
		if !taken(name) {
			return name
		}
	}
}
