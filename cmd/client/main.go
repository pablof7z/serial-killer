package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chzyer/readline"
	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip11"
)

// ANSI color codes
const (
	reset   = "\033[0m"
	bold    = "\033[1m"
	red     = "\033[31m"
	green   = "\033[32m"
	yellow  = "\033[33m"
	blue    = "\033[34m"
	magenta = "\033[35m"
	cyan    = "\033[36m"
	gray    = "\033[90m"
)

func c(color, s string) string { return color + s + reset }

// ClientState holds the REPL client's state.
type ClientState struct {
	secretKey nostr.SecretKey
	pool      *nostr.Pool
	ctx       context.Context
	cancel    context.CancelFunc
	relays    map[string]bool // connected relay URLs
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	cs := &ClientState{
		secretKey: nostr.Generate(),
		pool:      nostr.NewPool(),
		ctx:       ctx,
		cancel:    cancel,
		relays:    make(map[string]bool),
	}

	fmt.Printf("%s\n", c(bold+cyan, "=== Chain REPL Client ==="))
	fmt.Printf("Generated key: %s\n", c(yellow, cs.secretKey.Hex()))
	fmt.Printf("Pubkey:        %s\n", c(yellow, cs.secretKey.Public().Hex()))
	fmt.Printf("Type %s for commands.\n\n", c(bold, "help"))

	home, _ := os.UserHomeDir()
	historyFile := filepath.Join(home, ".serial-killer-history")

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          c(bold+cyan, "> "),
		HistoryFile:     historyFile,
		HistoryLimit:    1000,
		AutoComplete:    readline.NewPrefixCompleter(completers()...),
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize readline: %v\n", err)
		os.Exit(1)
	}
	defer rl.Close()

	for {
		line, err := rl.Readline()
		if err != nil { // io.EOF or readline.ErrInterrupt
			fmt.Println()
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		cmd := parts[0]
		args := parts[1:]

		switch cmd {
		case "help":
			printHelp()
		case "key":
			cs.handleKey(args)
		case "connect":
			cs.handleConnect(args)
		case "disconnect":
			cs.handleDisconnect(args)
		case "relays":
			cs.handleRelays()
		case "genesis":
			cs.handleGenesis(args)
		case "append":
			cs.handleAppend(args)
		case "query":
			cs.handleQuery(args)
		case "query-all":
			cs.handleQueryAll(args)
		case "diverge":
			cs.handleDiverge(args)
		case "fix":
			cs.handleFix(args)
		case "delete":
			cs.handleDelete(args)
		case "exit", "quit":
			fmt.Printf("%s\n", c(green, "Goodbye."))
			return
		default:
			fmt.Printf("%s: %s\n", c(red, "Unknown command"), cmd)
		}
	}
}

func completers() []readline.PrefixCompleterInterface {
	return []readline.PrefixCompleterInterface{
		readline.PcItem("help"),
		readline.PcItem("key"),
		readline.PcItem("connect"),
		readline.PcItem("disconnect"),
		readline.PcItem("relays"),
		readline.PcItem("genesis"),
		readline.PcItem("append"),
		readline.PcItem("query"),
		readline.PcItem("query-all"),
		readline.PcItem("diverge"),
		readline.PcItem("fix"),
		readline.PcItem("delete"),
		readline.PcItem("exit"),
		readline.PcItem("quit"),
	}
}

func printHelp() {
	fmt.Printf(`%s
  %s [hex]                    Show or set private key
  %s <url>                Connect to a relay
  %s <url>             Disconnect from a relay
  %s                       List connected relays
  %s [chain=NAME] [kind=N] <content>  Create a genesis event
  %s [chain=NAME] [kind=N] <content>   Append to a chain
  %s <relay> <chain>       Query a chain from a relay
  %s <chain>             Query a chain from all relays
  %s <chain>              Check for divergence across relays
  %s <relay> <chain> <canon>  Fix divergence on relay using canonical relay
  %s <relay> <chain> <id>   Delete a chain event on a relay
  %s                         Quit
`,
		c(bold, "Commands:"),
		c(cyan, "key"),
		c(cyan, "connect"),
		c(cyan, "disconnect"),
		c(cyan, "relays"),
		c(cyan, "genesis"),
		c(cyan, "append"),
		c(cyan, "query"),
		c(cyan, "query-all"),
		c(cyan, "diverge"),
		c(cyan, "fix"),
		c(cyan, "delete"),
		c(cyan, "exit"),
	)
}

func (cs *ClientState) handleKey(args []string) {
	if len(args) == 0 {
		fmt.Printf("%s %s\n", c(bold, "Private key:"), c(yellow, cs.secretKey.Hex()))
		fmt.Printf("%s %s\n", c(bold, "Pubkey:     "), c(yellow, cs.secretKey.Public().Hex()))
		return
	}
	sk, err := nostr.SecretKeyFromHex(args[0])
	if err != nil {
		fmt.Printf("%s: %v\n", c(red, "Invalid key"), err)
		return
	}
	cs.secretKey = sk
	fmt.Printf("%s Pubkey: %s\n", c(green, "Key set."), c(yellow, cs.secretKey.Public().Hex()))
}

func (cs *ClientState) handleConnect(args []string) {
	if len(args) < 1 {
		fmt.Printf("%s: %s\n", c(red, "Usage"), "connect <url>")
		return
	}
	url := normalizeURL(args[0])
	_, err := cs.pool.EnsureRelay(url)
	if err != nil {
		fmt.Printf("%s to %s: %v\n", c(red, "Failed to connect"), url, err)
		return
	}
	cs.relays[url] = true
	fmt.Printf("%s %s\n", c(green, "Connected to"), url)

	// Check NIP-11 for NIP-EC support
	info, err := nip11.Fetch(cs.ctx, url)
	if err != nil {
		fmt.Printf("%s: could not fetch NIP-11: %v\n", c(yellow, "Warning"), err)
		return
	}
	hasEC := false
	for _, nip := range info.SupportedNIPs {
		if nip == "EC" {
			hasEC = true
			break
		}
	}
	if !hasEC {
		fmt.Printf("%s: relay does not advertise NIP-EC support (chain enforcement may not work)\n", c(yellow, "Warning"))
	}
}

func (cs *ClientState) handleDisconnect(args []string) {
	if len(args) < 1 {
		fmt.Printf("%s: %s\n", c(red, "Usage"), "disconnect <url>")
		return
	}
	url := normalizeURL(args[0])
	delete(cs.relays, url)
	fmt.Printf("%s from %s (connection may persist in pool)\n", c(yellow, "Disconnected"), url)
}

func (cs *ClientState) handleRelays() {
	if len(cs.relays) == 0 {
		fmt.Printf("%s\n", c(yellow, "No relays connected."))
		return
	}
	fmt.Printf("%s\n", c(bold, "Connected relays:"))
	for url := range cs.relays {
		fmt.Printf("  %s\n", c(cyan, url))
	}
}

func (cs *ClientState) handleGenesis(args []string) {
	if len(args) < 1 {
		fmt.Printf("%s: %s\n", c(red, "Usage"), "genesis [chain=NAME] [kind=N] <content>")
		return
	}
	kind, chainName, content := parseCommandArgs(args)

	tags := nostr.Tags{{"chain"}}
	if chainName != "" {
		tags = append(tags, nostr.Tag{"d", chainName})
	}
	evt := nostr.Event{
		Kind:      kind,
		CreatedAt: nostr.Now(),
		Tags:      tags,
		Content:   content,
	}
	evt.PubKey = cs.secretKey.Public()
	if err := evt.Sign(cs.secretKey); err != nil {
		fmt.Printf("%s: %v\n", c(red, "Failed to sign"), err)
		return
	}

	cs.publishEvent(evt)
}

func (cs *ClientState) handleAppend(args []string) {
	if len(args) < 1 {
		fmt.Printf("%s: %s\n", c(red, "Usage"), "append [chain=NAME] [kind=N] <content>")
		return
	}
	kind, chainName, content := parseCommandArgs(args)

	// Find current head by querying all relays
	head := cs.findHead(chainName, kind)
	if head == "" {
		fmt.Printf("%s\n", c(yellow, "No active head found. Use 'genesis' to start a chain."))
		return
	}

	tags := nostr.Tags{{"chain"}}
	if chainName != "" {
		tags = append(tags, nostr.Tag{"d", chainName})
	}
	tags = append(tags, nostr.Tag{"prev", head})
	evt := nostr.Event{
		Kind:      kind,
		CreatedAt: nostr.Now(),
		Tags:      tags,
		Content:   content,
	}
	evt.PubKey = cs.secretKey.Public()
	if err := evt.Sign(cs.secretKey); err != nil {
		fmt.Printf("%s: %v\n", c(red, "Failed to sign"), err)
		return
	}

	cs.publishEvent(evt)
}

func (cs *ClientState) publishEvent(evt nostr.Event) {
	if len(cs.relays) == 0 {
		fmt.Printf("%s\n", c(yellow, "No relays connected."))
		return
	}

	urls := make([]string, 0, len(cs.relays))
	for url := range cs.relays {
		urls = append(urls, url)
	}

	fmt.Printf("Publishing event %s to %s relay(s)...\n", c(cyan, evt.ID.Hex()), c(bold, fmt.Sprintf("%d", len(urls))))
	ctx, cancel := context.WithTimeout(cs.ctx, 5*time.Second)
	defer cancel()

	for res := range cs.pool.PublishMany(ctx, urls, evt) {
		if res.Error != nil {
			fmt.Printf("  [%s] %s: %v\n", res.RelayURL, c(red, "FAILED"), res.Error)
		} else {
			fmt.Printf("  [%s] %s\n", res.RelayURL, c(green, "OK"))
		}
	}
}

func (cs *ClientState) handleQuery(args []string) {
	if len(args) < 2 {
		fmt.Printf("%s: %s\n", c(red, "Usage"), "query <relay-url> <chain-name>")
		return
	}
	relayURL := normalizeURL(args[0])
	chainName, kind := parseChainArg(args[1])

	chainEvents := cs.fetchChain([]string{relayURL}, chainName, kind)
	if len(chainEvents) == 0 {
		fmt.Printf("%s\n", c(yellow, "No events found."))
		return
	}
	printChain(chainEvents)
}

func (cs *ClientState) handleQueryAll(args []string) {
	if len(args) < 1 {
		fmt.Printf("%s: %s\n", c(red, "Usage"), "query-all <chain-name>")
		return
	}
	chainName, kind := parseChainArg(args[0])
	if len(cs.relays) == 0 {
		fmt.Printf("%s\n", c(yellow, "No relays connected."))
		return
	}

	urls := make([]string, 0, len(cs.relays))
	for url := range cs.relays {
		urls = append(urls, url)
	}

	for _, url := range urls {
		fmt.Printf("\n%s %s\n", c(bold+magenta, "=== Relay:"), c(bold+magenta, url))
		chainEvents := cs.fetchChain([]string{url}, chainName, kind)
		if len(chainEvents) == 0 {
			fmt.Printf("%s\n", c(yellow, "No events found."))
			continue
		}
		printChain(chainEvents)
	}
}

func (cs *ClientState) handleDiverge(args []string) {
	if len(args) < 1 {
		fmt.Printf("%s: %s\n", c(red, "Usage"), "diverge <chain-name>")
		return
	}
	chainName, kind := parseChainArg(args[0])
	if len(cs.relays) < 2 {
		fmt.Printf("%s\n", c(yellow, "Need at least 2 relays to check divergence."))
		return
	}

	urls := make([]string, 0, len(cs.relays))
	for url := range cs.relays {
		urls = append(urls, url)
	}

	// Fetch chain from each relay
	chains := make(map[string][]nostr.Event)
	for _, url := range urls {
		chains[url] = cs.fetchChain([]string{url}, chainName, kind)
	}

	// Find divergence
	fmt.Printf("Checking divergence for chain '%s'...\n\n", c(cyan, chainLabel(chainName, kind)))
	foundDivergence := false
	for i := 0; i < len(urls); i++ {
		for j := i + 1; j < len(urls); j++ {
			a, b := chains[urls[i]], chains[urls[j]]
			lastCommon := findLastCommonEvent(a, b)
			if lastCommon == "" {
				fmt.Printf("Relay %s and %s have %s\n", c(cyan, urls[i]), c(cyan, urls[j]), c(red, "no common events"))
				foundDivergence = true
			} else if lastCommon == "same" {
				fmt.Printf("Relay %s and %s are %s\n", c(cyan, urls[i]), c(cyan, urls[j]), c(green, "in sync"))
			} else {
				fmt.Printf("%s between %s and %s\n", c(red+bold, "DIVERGENCE"), c(cyan, urls[i]), c(cyan, urls[j]))
				fmt.Printf("  %s %s\n", c(yellow, "Last common:"), c(yellow, lastCommon))
				printBranch("  "+c(blue, "Branch A:")+" ", a, lastCommon)
				printBranch("  "+c(blue, "Branch B:")+" ", b, lastCommon)
				foundDivergence = true
			}
		}
	}

	if !foundDivergence {
		fmt.Printf("%s\n", c(green, "No divergence detected. All relays are in sync."))
	}
}

func (cs *ClientState) handleFix(args []string) {
	if len(args) < 3 {
		fmt.Printf("%s: %s\n", c(red, "Usage"), "fix <relay-url> <chain-name> <canonical-relay-url>")
		return
	}
	targetURL := normalizeURL(args[0])
	chainName, kind := parseChainArg(args[1])
	canonicalURL := normalizeURL(args[2])

	// Fetch from both
	targetChain := cs.fetchChain([]string{targetURL}, chainName, kind)
	canonicalChain := cs.fetchChain([]string{canonicalURL}, chainName, kind)

	if len(canonicalChain) == 0 {
		fmt.Printf("%s\n", c(yellow, "Canonical relay has no events for this chain."))
		return
	}

	lastCommon := findLastCommonEvent(targetChain, canonicalChain)
	if lastCommon == "same" {
		fmt.Printf("%s\n", c(green, "Relays are already in sync."))
		return
	}
	if lastCommon == "" {
		fmt.Printf("%s\n", c(red, "No common ancestor found. Cannot fix automatically."))
		return
	}

	fmt.Printf("%s %s\n", c(yellow, "Last common event:"), c(yellow, lastCommon))

	// Find the losing suffix on target
	losingSuffix := getBranchSuffix(targetChain, lastCommon)
	if len(losingSuffix) > 0 {
		fmt.Printf("Deleting %s event(s) from target relay...\n", c(red, fmt.Sprintf("%d", len(losingSuffix))))
		cs.deleteEvents(targetURL, losingSuffix)
	}

	// Find the winning suffix from canonical
	winningSuffix := getBranchSuffix(canonicalChain, lastCommon)
	if len(winningSuffix) > 0 {
		fmt.Printf("Replaying %s event(s) to target relay...\n", c(green, fmt.Sprintf("%d", len(winningSuffix))))
		for _, evt := range winningSuffix {
			ctx, cancel := context.WithTimeout(cs.ctx, 5*time.Second)
			res := cs.pool.PublishMany(ctx, []string{targetURL}, evt)
			for r := range res {
				if r.Error != nil {
					fmt.Printf("  %s %s: %v\n", c(red, "Failed to replay"), evt.ID.Hex(), r.Error)
				} else {
					fmt.Printf("  %s %s\n", c(green, "Replayed"), evt.ID.Hex())
				}
			}
			cancel()
		}
	}

	fmt.Printf("%s\n", c(green, "Done."))
}

func (cs *ClientState) handleDelete(args []string) {
	if len(args) < 3 {
		fmt.Printf("%s: %s\n", c(red, "Usage"), "delete <relay-url> <chain-name> <event-id>")
		return
	}
	relayURL := normalizeURL(args[0])
	_ = args[1] // chainName not needed for the actual delete event
	eventID := args[2]

	// Fetch the target event to get its kind
	filter := nostr.Filter{
		IDs: []nostr.ID{nostr.MustIDFromHex(eventID)},
	}
	ctx, cancel := context.WithTimeout(cs.ctx, 5*time.Second)
	var target *nostr.Event
	for ievt := range cs.pool.FetchMany(ctx, []string{relayURL}, filter, nostr.SubscriptionOptions{}) {
		target = &ievt.Event
		break
	}
	cancel()

	if target == nil {
		fmt.Printf("Event %s not found on relay\n", c(yellow, eventID))
		return
	}

	// Create kind 5 deletion request
	delEvt := nostr.Event{
		Kind:      5,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"e", eventID},
			{"k", fmt.Sprintf("%d", target.Kind)},
		},
		Content: "rollback chain event",
	}
	delEvt.PubKey = cs.secretKey.Public()
	if err := delEvt.Sign(cs.secretKey); err != nil {
		fmt.Printf("%s: %v\n", c(red, "Failed to sign"), err)
		return
	}

	ctx2, cancel2 := context.WithTimeout(cs.ctx, 5*time.Second)
	defer cancel2()
	for res := range cs.pool.PublishMany(ctx2, []string{relayURL}, delEvt) {
		if res.Error != nil {
			fmt.Printf("  [%s] %s %s: %v\n", res.RelayURL, c(red, "Delete"), eventID, res.Error)
		} else {
			fmt.Printf("  [%s] %s - %s\n", res.RelayURL, c(green, "OK"), "deletion request published")
		}
	}
}

// Helper: fetch all events for a chain from given relays.
func (cs *ClientState) fetchChain(urls []string, chainName string, kind nostr.Kind) []nostr.Event {
	filter := nostr.Filter{
		Authors: []nostr.PubKey{cs.secretKey.Public()},
	}
	if chainName != "" {
		filter.Tags = nostr.TagMap{"d": {chainName}}
	} else {
		filter.Kinds = []nostr.Kind{kind}
	}

	ctx, cancel := context.WithTimeout(cs.ctx, 5*time.Second)
	defer cancel()

	events := make(map[string]nostr.Event)
	for ievt := range cs.pool.FetchMany(ctx, urls, filter, nostr.SubscriptionOptions{}) {
		evt := ievt.Event
		hasChainTag := false
		for _, tag := range evt.Tags {
			if len(tag) >= 1 && tag[0] == "chain" {
				hasChainTag = true
				break
			}
		}
		if !hasChainTag {
			continue
		}
		events[ievt.ID.Hex()] = evt
	}

	// Build chain by following prev pointers from head
	// First find the head (event that no other event points to)
	hasPrev := make(map[string]bool)
	for _, evt := range events {
		for _, tag := range evt.Tags {
			if len(tag) >= 2 && tag[0] == "prev" {
				hasPrev[tag[1]] = true
			}
		}
	}

	// Find head(s)
	var headID string
	for id := range events {
		if !hasPrev[id] {
			headID = id
			break
		}
	}
	if headID == "" && len(events) > 0 {
		// All have prev, pick arbitrarily
		for id := range events {
			headID = id
			break
		}
	}

	// Build reverse chain (head -> genesis)
	var chain []nostr.Event
	visited := make(map[string]bool)
	for headID != "" {
		if visited[headID] {
			break // cycle detected
		}
		visited[headID] = true
		evt, ok := events[headID]
		if !ok {
			break
		}
		chain = append(chain, evt)

		// Find prev
		prev := ""
		for _, tag := range evt.Tags {
			if len(tag) >= 2 && tag[0] == "prev" {
				prev = tag[1]
				break
			}
		}
		headID = prev
	}

	// Reverse to get genesis -> head
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}

	return chain
}

func (cs *ClientState) findHead(chainName string, kind nostr.Kind) string {
	if len(cs.relays) == 0 {
		return ""
	}
	urls := make([]string, 0, len(cs.relays))
	for url := range cs.relays {
		urls = append(urls, url)
	}
	chainEvents := cs.fetchChain(urls, chainName, kind)
	if len(chainEvents) == 0 {
		return ""
	}
	return chainEvents[len(chainEvents)-1].ID.Hex()
}

// deleteEvents deletes events tip-first (reverse order) and stops on first error.
func (cs *ClientState) deleteEvents(relayURL string, events []nostr.Event) {
	for i := len(events) - 1; i >= 0; i-- {
		evt := events[i]
		delEvt := nostr.Event{
			Kind:      5,
			CreatedAt: nostr.Now(),
			Tags: nostr.Tags{
				{"e", evt.ID.Hex()},
				{"k", fmt.Sprintf("%d", evt.Kind)},
			},
			Content: "rollback chain event",
		}
		delEvt.PubKey = cs.secretKey.Public()
		if err := delEvt.Sign(cs.secretKey); err != nil {
			fmt.Printf("  %s %s: %v\n", c(red, "Failed to sign deletion for"), evt.ID.Hex(), err)
			return
		}

		failed := false
		ctx, cancel := context.WithTimeout(cs.ctx, 5*time.Second)
		for res := range cs.pool.PublishMany(ctx, []string{relayURL}, delEvt) {
			if res.Error != nil {
				fmt.Printf("  [%s] %s %s: %v\n", res.RelayURL, c(red, "Delete failed"), evt.ID.Hex(), res.Error)
				failed = true
			} else {
				fmt.Printf("  [%s] %s %s\n", res.RelayURL, c(green, "Deleted"), evt.ID.Hex())
			}
		}
		cancel()
		if failed {
			return
		}
	}
}

// printChain prints a chain from genesis to head.
func printChain(events []nostr.Event) {
	for i, evt := range events {
		prefix := "  "
		color := ""
		label := ""
		if i == 0 {
			prefix = ""
			color = green
			label = "G "
		} else if i == len(events)-1 {
			prefix = ""
			color = cyan
			label = "H "
		}
		idShort := evt.ID.Hex()
		if len(idShort) > 16 {
			idShort = idShort[:16] + "..."
		}
		fmt.Printf("%s%s%s (kind=%s, content=%s)\n", prefix, c(color, label+idShort), reset, c(yellow, fmt.Sprintf("%d", evt.Kind)), c(gray, fmt.Sprintf("%q", evt.Content)))
	}
}

// findLastCommonEvent finds the last common event ID between two chains.
// Returns "" if no common events, "same" if fully identical.
func findLastCommonEvent(a, b []nostr.Event) string {
	if len(a) == 0 || len(b) == 0 {
		return ""
	}

	// Build ID sets
	bIDs := make(map[string]int) // index in b
	for i, evt := range b {
		bIDs[evt.ID.Hex()] = i
	}

	lastCommon := ""
	for _, evt := range a {
		if _, ok := bIDs[evt.ID.Hex()]; ok {
			lastCommon = evt.ID.Hex()
		}
	}

	if lastCommon == "" {
		return ""
	}

	// Check if fully identical
	if len(a) == len(b) {
		allSame := true
		for i := range a {
			if a[i].ID != b[i].ID {
				allSame = false
				break
			}
		}
		if allSame {
			return "same"
		}
	}

	return lastCommon
}

// printBranch prints the suffix of a chain after a given event ID.
func printBranch(label string, events []nostr.Event, afterID string) {
	found := false
	var ids []string
	for _, evt := range events {
		if found {
			ids = append(ids, evt.ID.Hex())
		}
		if evt.ID.Hex() == afterID {
			found = true
		}
	}
	if len(ids) > 0 {
		fmt.Printf("%s%s\n", label, c(red, strings.Join(ids, " -> ")))
	}
}

// getBranchSuffix returns events in the chain after the given event ID.
func getBranchSuffix(events []nostr.Event, afterID string) []nostr.Event {
	var result []nostr.Event
	found := false
	for _, evt := range events {
		if found {
			result = append(result, evt)
		}
		if evt.ID.Hex() == afterID {
			found = true
		}
	}
	return result
}

func normalizeURL(url string) string {
	if !strings.HasPrefix(url, "ws://") && !strings.HasPrefix(url, "wss://") {
		url = "wss://" + url
	}
	return nostr.NormalizeURL(url)
}

// parseCommandArgs parses genesis/append arguments, supporting both styles:
//   named:      [kind=N] [chain=NAME] <content>
//   positional: <chain-name> [kind] <content>  (backwards compat)
//
// If chain is not specified, it defaults to the kind number as a string,
// which matches the implicit chain name for replaceable events.
func parseCommandArgs(args []string) (kind nostr.Kind, chainName string, content string) {
	kind = 1
	var rest []string
	hasNamedArgs := false
	for _, arg := range args {
		k, v, ok := strings.Cut(arg, "=")
		if !ok {
			rest = append(rest, arg)
			continue
		}
		switch k {
		case "kind":
			if n, err := strconv.Atoi(v); err == nil && n >= 0 && n != 5 {
				kind = nostr.Kind(n)
				hasNamedArgs = true
			} else {
				rest = append(rest, arg)
			}
		case "chain":
			chainName = v
			hasNamedArgs = true
		default:
			rest = append(rest, arg)
		}
	}
	if !hasNamedArgs {
		// Positional chain name: first non-numeric remaining arg when chain not set
		if chainName == "" && len(rest) > 0 {
			if _, err := strconv.Atoi(rest[0]); err != nil {
				chainName = rest[0]
				rest = rest[1:]
			}
		}
		// Positional kind: first numeric remaining arg
		if len(rest) > 0 {
			if n, err := strconv.Atoi(rest[0]); err == nil && n >= 0 && n != 5 {
				kind = nostr.Kind(n)
				rest = rest[1:]
			}
		}
	}
	content = strings.Join(rest, " ")
	return
}

// parseChainArg converts a user-supplied chain identifier:
// numeric → (empty chainName, kind N) — implicit chain identified by kind
// string  → (chainName, 1)            — named chain identified by d tag
func parseChainArg(arg string) (chainName string, kind nostr.Kind) {
	if n, err := strconv.Atoi(arg); err == nil {
		return "", nostr.Kind(n)
	}
	return arg, 1
}

// chainLabel returns a human-readable chain identifier for display.
func chainLabel(chainName string, kind nostr.Kind) string {
	if chainName != "" {
		return chainName
	}
	return strconv.Itoa(int(kind))
}
