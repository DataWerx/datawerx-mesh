package meshfw

// Access is one permitted reachability extracted from a compiled Ruleset:
// traffic from Source to Dest on Protocol/Port is accepted. An empty field means
// "any" (the rule carried no -s, -d, -p, or --dport for it). It is this package's
// stable, data-plane-consistent answer to "what does this policy set actually
// permit", so the impact and reachability analyzers compose it instead of
// re-deriving firewall semantics.
type Access struct {
	Source   string
	Dest     string
	Protocol string
	Port     string
}

// Decisions is the structured interpretation of a compiled Ruleset: the
// permitted accesses and the destinations placed under default-deny. Both
// preserve the Ruleset's deterministic order with duplicates removed.
type Decisions struct {
	Accepts   []Access
	Protected []string
}

// Interpret reads a compiled Ruleset back into its accept/protected decisions.
// The arg-vector format is this package's own stable output, so interpreting it
// here keeps every consumer consistent with exactly what the data plane
// programs. The leading conntrack RELATED,ESTABLISHED fast-path carries no
// -s/-d and is skipped.
func Interpret(rs Ruleset) Decisions {
	var d Decisions
	seenAccept := map[Access]bool{}
	seenProtected := map[string]bool{}
	for _, r := range rs.Rules {
		if len(r.Args) == 0 || hasArg(r.Args, "conntrack") {
			continue
		}
		switch r.Args[len(r.Args)-1] {
		case "ACCEPT":
			a := Access{
				Source:   argValue(r.Args, "-s"),
				Dest:     argValue(r.Args, "-d"),
				Protocol: argValue(r.Args, "-p"),
				Port:     argValue(r.Args, "--dport"),
			}
			if !seenAccept[a] {
				seenAccept[a] = true
				d.Accepts = append(d.Accepts, a)
			}
		case "DROP":
			dst := argValue(r.Args, "-d")
			if !seenProtected[dst] {
				seenProtected[dst] = true
				d.Protected = append(d.Protected, dst)
			}
		}
	}
	return d
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func argValue(args []string, flag string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}
