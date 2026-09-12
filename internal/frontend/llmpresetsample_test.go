package frontend_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/domain"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
	"github.com/0xGosu/herdr-auto-pilot/internal/llm"
	"github.com/BurntSushi/toml"
)

// sampleRecipeField maps a sample/config.toml key to the preset key it
// bootstraps. The orchestrator sits under [full_self_prompting] rather than
// [llm]; the scanner below is line-based and does not care, which is the whole
// reason it can cover all five.
var sampleRecipeField = map[string]string{
	"command":                    frontend.LLMCommandKey,
	"task_generate_command":      frontend.LLMTaskGenerateCommandKey,
	"learn_from_user_command":    frontend.LLMLearnFromUserCommandKey,
	"reranking_command":          frontend.LLMRerankingCommandKey,
	"orchestrator_agent_command": frontend.FSPOrchestratorCommandFieldKey,
}

// A recipe assignment, active or commented out. The optional "# " is what
// makes a commented recipe readable; the sample writes the [llm] ones as
// "# key = [" and the orchestrator one as "#key = [...]" on a single line.
var sampleRecipeStart = regexp.MustCompile(`^#?\s?(` +
	`command|task_generate_command|learn_from_user_command|reranking_command|orchestrator_agent_command` +
	`) = \[`)

// sampleRecipes reads every recipe assignment out of sample/config.toml,
// including the commented-out ones, and returns them keyed by preset key and
// then by the CLI its argv[0] names.
//
// The stripping is deliberately dumb — drop a leading "#" and at most one
// space — because that is exactly the transformation an operator performs when
// they "uncomment the Codex ones to switch". Anything cleverer would let the
// file drift into a shape the operator's own edit does not produce. The
// stripped block is then handed to the REAL TOML decoder rather than compared
// as text: a recipe's own inner "# …" notes survive as TOML comments, and the
// quoting of a prompt carrying apostrophes, embedded double quotes and real
// newlines is a thing only a decoder gets right.
func sampleRecipes(t *testing.T) map[string]map[string][]string {
	t.Helper()
	path := filepath.Join("..", "..", "sample", "config.toml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	out := map[string]map[string][]string{}

	uncomment := func(s string) string {
		s = strings.TrimPrefix(s, "#")
		return strings.TrimPrefix(s, " ")
	}

	for i := 0; i < len(lines); i++ {
		m := sampleRecipeStart.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		field := m[1]
		commented := strings.HasPrefix(lines[i], "#")
		block := []string{uncomment(lines[i])}
		// A single-line assignment closes on its own line.
		if !strings.HasSuffix(strings.TrimSpace(lines[i]), "]") {
			closer := "]"
			if commented {
				closer = "# ]"
			}
			for i++; i < len(lines); i++ {
				block = append(block, uncomment(lines[i]))
				if strings.TrimRight(lines[i], " \t") == closer {
					break
				}
			}
			if i == len(lines) {
				t.Fatalf("%s at line %d is never closed", field, i)
			}
		}
		var decoded map[string][]string
		if _, err := toml.Decode(strings.Join(block, "\n"), &decoded); err != nil {
			t.Fatalf("%s near line %d does not parse once uncommented: %v\n%s",
				field, i, err, strings.Join(block, "\n"))
		}
		argv := decoded[field]
		if len(argv) == 0 {
			t.Fatalf("%s near line %d decoded to nothing", field, i)
		}
		key := sampleRecipeField[field]
		if out[key] == nil {
			out[key] = map[string][]string{}
		}
		cli := filepath.Base(argv[0])
		if _, dup := out[key][cli]; dup {
			t.Fatalf("sample/config.toml carries two %s recipes for %s — "+
				"the drift guard cannot tell which one the preset should match", field, cli)
		}
		out[key][cli] = argv
	}
	return out
}

// TestEveryPresetHasAByteIdenticalTwinInTheSample is the drift guard the
// registry's own doc comment promises ("copied VERBATIM from
// sample/config.toml").
//
// TestSamplePresetsMatchTheGoRecipes covers only the two recipes the sample
// leaves ACTIVE, which is both claude ones and neither codex nor agy one. That
// left six of the eight recipes held together by prose alone, and agy adds
// three more — all commented, so the old test would cover none of them. A
// preset is argv an operator installs with one keystroke and never reads, so
// the sample IS its documentation: the two silently disagreeing means an
// operator reads one recipe and runs another.
//
// The comparison runs BOTH ways on purpose. A preset with no twin in the file
// is an undocumented recipe; a recipe in the file with no preset is a promise
// the picker does not keep.
func TestEveryPresetHasAByteIdenticalTwinInTheSample(t *testing.T) {
	sample := sampleRecipes(t)

	checked := 0
	for _, key := range frontend.LLMPresetKeys {
		for _, preset := range frontend.LLMPresetNamesFor(key) {
			want, ok := frontend.LLMPreset(key, preset)
			if !ok {
				t.Fatalf("LLMPresetNamesFor(%s) offered %s but LLMPreset has none", key, preset)
			}
			// Every preset name is also the binary its recipe invokes, which is
			// what lets a recipe in the file be attributed to a preset at all.
			if filepath.Base(want[0]) != preset {
				t.Fatalf("%s/%s invokes %q, not %q — the sample scanner keys on argv[0] "+
					"and can no longer attribute this recipe", key, preset, want[0], preset)
			}
			got, ok := sample[key][preset]
			if !ok {
				t.Errorf("%s has a %s preset with no twin in sample/config.toml — "+
					"add the recipe there (commented, in the style of the codex ones)", key, preset)
				continue
			}
			checked++
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s/%s: sample/config.toml and the preset have drifted\nsample %#v\npreset %#v",
					key, preset, got, want)
			}
		}
	}
	for key, byCLI := range sample {
		for cli := range byCLI {
			if _, ok := frontend.LLMPreset(key, cli); !ok {
				t.Errorf("sample/config.toml documents a %s recipe for %s that no preset installs — "+
					"either register it or the docs promise something the picker will not do", key, cli)
			}
		}
	}
	// Guards the scanner: a renamed field or a regexp that stopped matching
	// would otherwise make this pass by comparing nothing.
	if checked < len(frontend.LLMPresetKeys) {
		t.Fatalf("only %d recipes were compared against the sample, fewer than the %d preset keys — "+
			"fix the scanner rather than deleting the test", checked, len(frontend.LLMPresetKeys))
	}
}

// TestTheAgyPresetsTakeTheNarrowestGrantAgyOffers pins the three claims about
// agy that a reader cannot check by looking at the argv.
//
// agy has exactly ONE permission flag and it is all-or-nothing: tools, file
// writes and shell, in the MONITORED AGENT's own directory. There is no
// --permission-mode acceptEdits to step down to (what the claude learn recipe
// uses) and no --sandbox read-only floor (what the codex generate and judge
// recipes use). So "as tight as agy allows" reduces to a single question per
// recipe — does this run write a file? — and only the learn recipe answers yes.
//
// Each assertion below is a decision that was made against evidence, not a
// preference, and each fails in a different direction if it is undone:
//
//   - Adding the flag to generate or rerank widens a grant that buys nothing.
//     Measured on agy 1.2.2: the generate recipe answered cleanly 13 times
//     WITHOUT it across two models, and 2 runs WITH it ignored a deliberately
//     loud AUTO.md exactly as the flagless runs did.
//   - Removing it from learn breaks that recipe outright — headless agy denies
//     every tool without it, and this is the one recipe that must write.
//   - Reaching for --sandbox as the "safer" middle ground is the trap: under
//     `--sandbox --dangerously-skip-permissions` a write was silently dropped
//     while the model still answered "DONE", and a read of a file that was
//     present came back "NOFILE". A flag that fails by lying is worse than no
//     flag, so no agy recipe may carry it.
//   - --disable-slash-commands is the only real scoping flag agy has, and all
//     three runs are handed untrusted pane text in a repo hap does not control.
func TestTheAgyPresetsTakeTheNarrowestGrantAgyOffers(t *testing.T) {
	const skipPerms = "--dangerously-skip-permissions"
	wantsWriteAccess := map[string]bool{
		frontend.LLMTaskGenerateCommandKey:  false,
		frontend.LLMLearnFromUserCommandKey: true,
		frontend.LLMRerankingCommandKey:     false,
	}
	for key, wantFlag := range wantsWriteAccess {
		argv, ok := frontend.LLMPreset(key, frontend.LLMPresetAgy)
		if !ok {
			t.Errorf("%s lost its agy preset", key)
			continue
		}
		if got := slices.Contains(argv, skipPerms); got != wantFlag {
			if wantFlag {
				t.Errorf("%s: the agy learn recipe dropped %s — it is the ONLY recipe that "+
					"writes a file, and headless agy denies every tool without it", key, skipPerms)
			} else {
				t.Errorf("%s: the agy recipe took %s, agy's all-or-nothing grant (tools, "+
					"writes AND shell in the monitored agent's project). This run writes "+
					"nothing, and the flag was measured to buy it nothing.", key, skipPerms)
			}
		}
		if slices.Contains(argv, "--sandbox") {
			t.Errorf("%s: the agy recipe took --sandbox, which silently drops writes and "+
				"reads while the model still reports success — it is not a narrower grant", key)
		}
		if !slices.Contains(argv, "--disable-slash-commands") {
			t.Errorf("%s: the agy recipe dropped --disable-slash-commands, the only scoping "+
				"flag agy offers; these runs carry untrusted pane text into a repo hap "+
				"does not control", key)
		}
	}
	// The two keys agy CANNOT serve, asserted rather than left to the docs: an
	// agy recipe appearing here would install argv that fails opaquely.
	for _, key := range []string{frontend.LLMCommandKey, frontend.FSPOrchestratorCommandFieldKey} {
		if _, ok := frontend.LLMPreset(key, frontend.LLMPresetAgy); ok {
			t.Errorf("%s gained an agy preset. The consult needs hap's MCP server and agy "+
				"has no per-invocation MCP flag; the orchestrator's argv[0] is a herdr "+
				"agent KIND and only %q is supported. Read LLMPresetAgy before adding one.",
				key, domain.OrchestratorAgentKind)
		}
		if slices.Contains(frontend.LLMPresetNamesFor(key), frontend.LLMPresetAgy) {
			t.Errorf("the picker offers agy for %s, which has no agy recipe", key)
		}
	}
}

// TestEveryAgyPresetSurvivesTheArgvNormalizer is the agy half of
// TestEveryClaudePresetSurvivesTheArgvNormalizer, which skips every recipe
// whose argv[0] is not "claude" and so covers none of these.
//
// It matters for the same reason: fixPromptAdjacency bails out ENTIRELY and
// silently on a flag no map in normalize.go classifies, so a preset that gains
// one loses prompt repair with nothing pointing at it. --disable-slash-commands
// is new to agyBoolFlags for exactly these recipes.
//
// Two assertions, and the second is the discriminating one: every recipe here
// already writes its prompt immediately after -p, so normalizing it returns at
// the adjacency early-out and would pass with the flag maps empty.
func TestEveryAgyPresetSurvivesTheArgvNormalizer(t *testing.T) {
	checked := 0
	for _, key := range frontend.LLMPresetKeys {
		argv, ok := frontend.LLMPreset(key, frontend.LLMPresetAgy)
		if !ok {
			continue
		}
		checked++
		if got := llm.NormalizeLLMCommand(argv); !reflect.DeepEqual(got, argv) {
			t.Errorf("%s: the normalizer rewrote a correct agy recipe\n got %v\nwant %v", key, got, argv)
		}
		promptAt := -1
		for i, a := range argv {
			if a == "-p" {
				promptAt = i + 1
				break
			}
		}
		if promptAt <= 0 || promptAt >= len(argv) {
			t.Errorf("%s: agy preset has no prompt after -p", key)
			continue
		}
		// Every agy recipe already ENDS with its prompt, so the claude test's
		// "move the prompt to the end" is a no-op here and would pass with the
		// agy flag maps empty. Rearrange into the shape an operator actually
		// produces when they copy the flags and append their own prompt:
		// `agy -p <flags…> <prompt>`.
		prompt := argv[promptAt]
		moved := []string{argv[0], "-p"}
		for i := 1; i < len(argv); i++ {
			if i == promptAt-1 || i == promptAt { // the -p and the prompt
				continue
			}
			moved = append(moved, argv[i])
		}
		moved = append(moved, prompt)
		if fixed := llm.NormalizeLLMCommand(moved); len(fixed) < 3 || fixed[2] != prompt {
			t.Errorf("%s: a prompt moved to the end was not repaired back next to -p — "+
				"the repair bailed on a flag no agy map in normalize.go classifies\n got %v", key, fixed)
		}
	}
	if checked == 0 {
		t.Fatal("no agy preset was checked — fix the walk rather than deleting the test")
	}
}
