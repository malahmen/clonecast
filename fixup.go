package main

// Reinstall the freshly built agent into every running prefix and start it —
// exactly what the Prefixes pane now does on `i`.
import (
	"fmt"

	"github.com/malahmen/clonecast/internal/prefix"
)

func main() {
	all, err := prefix.DiscoverAll()
	if err != nil {
		fmt.Println("discover:", err)
	}
	for _, p := range all {
		if !p.Running() {
			fmt.Printf("  skip (idle)   %s\n", p.Prefix)
			continue
		}
		wine := prefix.WineCmdFromProcesses(p)
		if wine == "" {
			fmt.Printf("  skip (no wine) %s\n", p.Prefix)
			continue
		}
		cfg := prefix.Config{Prefix: p.Prefix, ExePath: "bin/clonecast-agent.exe", Port: 48800, WineCmd: wine}
		res, err := prefix.Install(cfg)
		if err != nil {
			fmt.Printf("  install FAILED %s: %v\n", p.Prefix, err)
			continue
		}
		serr := prefix.StartAgent(cfg)
		fmt.Printf("  %-12s route=%-5s started=%v %v\n", p.Game(), res.Route, serr == nil, serr)
	}
}
