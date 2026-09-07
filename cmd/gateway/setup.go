package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/erickardus/ai-gateway/internal/ccsettings"
)

// setupTimeout bounds the one call this command makes to the gateway.
const setupTimeout = 10 * time.Second

// modelChoice is one row of GET /v1/models, and one row of the picker.
type modelChoice struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
}

// runSetup points a project's Claude Code at this gateway.
//
// It exists because the settings that make Claude Code work through a gateway
// are not guessable and fail quietly when wrong. Three of them are the reason
// this is a command rather than a paragraph in a README:
//
//   - The key must travel in ANTHROPIC_CUSTOM_HEADERS. Every other way of
//     giving Claude Code a credential displaces a claude.ai subscription login,
//     which is the thing the gateway exists to preserve.
//   - A gateway model name is unresolvable to Claude Code unless it is
//     registered in ANTHROPIC_CUSTOM_MODEL_OPTION, and an unresolvable name is
//     not an error: the request silently goes to the saved default instead. A
//     developer sees answers from a model they did not choose and has no
//     reason to suspect the settings file.
//   - The model names themselves come from the gateway's own configuration, so
//     they cannot be listed in advance — which is why they are fetched and
//     offered rather than typed from memory.
//
// It writes a project directory by default, not the user-level one. A model
// pinned in ~/.claude applies to every checkout on the machine, and a harness
// for one gateway is not a decision about all of someone's work.
func runSetup(args []string) error {
	fs := flag.NewFlagSet("gateway setup", flag.ContinueOnError)
	gateway := fs.String("gateway", "http://127.0.0.1:4000", "base URL of the gateway")
	dir := fs.String("dir", ".claude", "Claude Code settings directory to write")
	key := fs.String("key", "", "virtual key; defaults to $GATEWAY_KEY, then to the key already in the settings file")
	model := fs.String("model", "", "model to use, skipping the picker")
	smallFast := fs.String("small-fast-model", "", "model for Claude Code's background work (default: the same model)")
	pickerModel := fs.String("picker-model", "", "gateway model to register for /model; \"none\" registers nothing")
	label := fs.String("label", "", "label for the registered model in the picker")
	yes := fs.Bool("yes", false, "do not prompt; requires -model")
	if err := fs.Parse(args); err != nil {
		return err
	}

	base, err := normalizeGateway(*gateway)
	if err != nil {
		return err
	}

	settingsPath := ccsettings.SettingsPath(*dir)
	f, err := ccsettings.Load(settingsPath)
	if err != nil {
		return err
	}
	// The same refusal gateway login makes, for the same reason: writing a
	// gateway key beside a credential that displaces the subscription produces
	// a configuration that works and quietly bills per token.
	if conflicts := f.Conflicts(); len(conflicts) > 0 {
		return conflictError(settingsPath, conflicts)
	}

	resolvedKey, err := resolveKey(*key, f)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	choices, err := fetchModels(ctx, base, resolvedKey)
	if err != nil {
		return err
	}
	if len(choices) == 0 {
		return fmt.Errorf("%s serves no model groups; add a model_list entry to its configuration first", base)
	}

	in := bufio.NewReader(os.Stdin)
	out := os.Stdout

	main, err := chooseModel(in, out, "Which model should this project use?", choices, *model, 0, *yes)
	if err != nil {
		return err
	}
	// Defaulting the background model to the chosen one keeps a first run to a
	// single decision. It is a separate variable because the cheap model for
	// session titles is rarely the one someone wants answering them.
	background := main
	if *smallFast != "" {
		if background, err = resolveNamed(*smallFast, choices); err != nil {
			return err
		}
	} else if !*yes {
		if background, err = chooseModel(in, out, "Which model for Claude Code's own background work?", choices, "", indexOfID(choices, main), false); err != nil {
			return err
		}
	}

	registered, err := choosePickerModel(in, out, choices, main, *pickerModel, *yes)
	if err != nil {
		return err
	}

	vars := map[string]string{
		ccsettings.EnvBaseURL:        base,
		ccsettings.EnvModel:          main,
		ccsettings.EnvSmallFastModel: background,
		ccsettings.EnvCustomHeaders:  "x-gateway-key: " + resolvedKey,
	}
	cleared := false
	if registered == "" {
		// Cleared rather than left behind: a stale registration names a model
		// this project no longer uses, and the picker would keep offering it.
		cleared = f.RemoveEnv(ccsettings.EnvCustomModelOption,
			ccsettings.EnvCustomModelOptionName,
			ccsettings.EnvCustomModelOptionDescription)
	} else {
		name := strings.TrimSpace(*label)
		if name == "" {
			name = prettyLabel(registered)
			if !*yes {
				if name, err = ask(in, out, "Label for "+registered+" in the model picker", name); err != nil {
					return err
				}
			}
		}
		vars[ccsettings.EnvCustomModelOption] = registered
		vars[ccsettings.EnvCustomModelOptionName] = name
		vars[ccsettings.EnvCustomModelOptionDescription] = registered + ", through the gateway at " + base
	}

	// Both halves are evaluated: SetEnv must run whether or not anything was
	// cleared, so it cannot be short-circuited by the || .
	changed := f.SetEnv(vars)
	if changed || cleared {
		if err := f.Save(); err != nil {
			return err
		}
	}

	fmt.Fprintf(out, "\nWrote %s\n", settingsPath)
	fmt.Fprintf(out, "  gateway          %s\n", base)
	fmt.Fprintf(out, "  model            %s\n", main)
	fmt.Fprintf(out, "  background model %s\n", background)
	if registered != "" {
		fmt.Fprintf(out, "  in /model as     %s\n", vars[ccsettings.EnvCustomModelOptionName])
	}
	if registered == "" && needsRegistration(main) {
		// Worth saying plainly, because the failure it predicts is silent.
		fmt.Fprintf(out, "\nNote: %s is not registered for the picker, so switching away from it\n"+
			"in /model is one-way for this session.\n", main)
	}
	fmt.Fprintf(out, "\nStart Claude Code in this directory. Its /login must be a claude.ai\n"+
		"subscription for passthrough deployments to bill the way you expect.\n")
	return nil
}

// normalizeGateway validates the gateway URL and strips a trailing slash, so
// that joining a path to it cannot produce a double slash.
func normalizeGateway(raw string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("-gateway must be an absolute URL such as http://127.0.0.1:4000, got %q", raw)
	}
	return trimmed, nil
}

// resolveKey finds the virtual key to write.
//
// Reusing the key already in the settings file is what lets this be run again
// to change models without the developer having to find their credential a
// second time — and without it being retyped into a shell history.
func resolveKey(flagValue string, f *ccsettings.File) (string, error) {
	if k := strings.TrimSpace(flagValue); k != "" {
		return k, nil
	}
	if k := strings.TrimSpace(os.Getenv("GATEWAY_KEY")); k != "" {
		return k, nil
	}
	if k := f.GatewayKey(); k != "" {
		return k, nil
	}
	return "", errors.New("no virtual key: pass -key, set GATEWAY_KEY, or run `gateway login` first")
}

// fetchModels asks the gateway which model groups it serves.
func fetchModels(ctx context.Context, base, key string) ([]modelChoice, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	// The default of the configurable virtual_keys.header_names. A gateway that
	// has renamed it away from this default is not one this command can talk to.
	req.Header.Set("x-gateway-key", key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ask %s which models it serves: %w", base, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d listing models: %s", base, resp.StatusCode, gatewayMessage(body))
	}
	var payload struct {
		Data []modelChoice `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode the model list from %s: %w", base, err)
	}
	return payload.Data, nil
}

// chooseModel resolves a model from the flag, or asks.
func chooseModel(in *bufio.Reader, out io.Writer, prompt string, choices []modelChoice, flagValue string, def int, noPrompt bool) (string, error) {
	if v := strings.TrimSpace(flagValue); v != "" {
		return resolveNamed(v, choices)
	}
	if noPrompt {
		if def >= 0 && def < len(choices) {
			return choices[def].ID, nil
		}
		return "", errors.New("-yes needs -model, since there is nothing to prompt with")
	}

	fmt.Fprintf(out, "\n%s\n\n", prompt)
	for i, c := range choices {
		marker := " "
		if i == def {
			marker = "*"
		}
		fmt.Fprintf(out, " %s %2d) %-32s %s\n", marker, i+1, c.ID, c.Description)
	}
	answer, err := ask(in, out, fmt.Sprintf("Choose 1-%d", len(choices)), strconv.Itoa(def+1))
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(strings.TrimSpace(answer))
	if err != nil || n < 1 || n > len(choices) {
		return "", fmt.Errorf("%q is not one of 1-%d", answer, len(choices))
	}
	return choices[n-1].ID, nil
}

// choosePickerModel decides which gateway model gets the one registration slot.
//
// It is asked separately from the model choice because the two answers differ
// in the case this command exists for: someone whose project model is a gateway
// model wants to switch to Sonnet and back, and it is the gateway model — not
// the one they are switching to — that has to be registered for the return trip
// to be possible.
func choosePickerModel(in *bufio.Reader, out io.Writer, choices []modelChoice, main, flagValue string, noPrompt bool) (string, error) {
	if v := strings.TrimSpace(flagValue); v != "" {
		if v == "none" {
			return "", nil
		}
		return resolveNamed(v, choices)
	}

	var custom []modelChoice
	for _, c := range choices {
		if needsRegistration(c.ID) {
			custom = append(custom, c)
		}
	}
	if len(custom) == 0 {
		return "", nil
	}
	def := ""
	if needsRegistration(main) {
		def = main
	} else {
		def = custom[0].ID
	}
	if noPrompt {
		return def, nil
	}

	fmt.Fprintf(out, "\nClaude Code resolves only its own model names. One gateway model can be\n"+
		"registered so that /model will switch back to it; the rest stay reachable\n"+
		"only by being the project default.\n\n")
	for i, c := range custom {
		fmt.Fprintf(out, "   %2d) %s\n", i+1, c.ID)
	}
	fmt.Fprintf(out, "   %2d) none\n", len(custom)+1)

	answer, err := ask(in, out, fmt.Sprintf("Register which? 1-%d", len(custom)+1), strconv.Itoa(indexOfID(custom, def)+1))
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(strings.TrimSpace(answer))
	if err != nil || n < 1 || n > len(custom)+1 {
		return "", fmt.Errorf("%q is not one of 1-%d", answer, len(custom)+1)
	}
	if n == len(custom)+1 {
		return "", nil
	}
	return custom[n-1].ID, nil
}

// needsRegistration reports whether Claude Code will fail to resolve a name.
//
// Claude Code resolves its own model ids, which all begin "claude-", and treats
// anything else as unknown unless it has been registered. The test is a prefix
// rather than a list because the list is Claude Code's and changes without this
// gateway hearing about it; the cost of being wrong in the safe direction is a
// registration that was not needed.
func needsRegistration(id string) bool {
	return !strings.HasPrefix(strings.ToLower(strings.TrimSpace(id)), "claude-")
}

// resolveNamed checks a model name against what the gateway actually serves,
// so a typo is caught here rather than becoming a 404 at the first prompt.
func resolveNamed(name string, choices []modelChoice) (string, error) {
	for _, c := range choices {
		if c.ID == name {
			return c.ID, nil
		}
	}
	ids := make([]string, 0, len(choices))
	for _, c := range choices {
		ids = append(ids, c.ID)
	}
	return "", fmt.Errorf("the gateway serves no model %q; it serves: %s", name, strings.Join(ids, ", "))
}

func indexOfID(choices []modelChoice, id string) int {
	for i, c := range choices {
		if c.ID == id {
			return i
		}
	}
	return 0
}

// prettyLabel turns a model id into something readable in a picker row.
func prettyLabel(id string) string {
	parts := strings.FieldsFunc(id, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// ask prints a prompt carrying its default and reads one line. An empty answer
// takes the default, which is what makes a run of this command mostly Enter.
func ask(in *bufio.Reader, out io.Writer, prompt, def string) (string, error) {
	fmt.Fprintf(out, "\n%s [%s]: ", prompt, def)
	line, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read answer: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}
