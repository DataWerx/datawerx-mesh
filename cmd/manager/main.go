// Command manager is the DataWerx Mesh node agent entry point. It is a thin
// shell over pkg/agent, which holds the entire bootstrap as an importable
// package. The open-core operator passes the zero Options, so it wires nothing
// commercial; the premium operator lives in its own repository and calls
// agent.Run with a premium registrar set.
package main

import (
	"os"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/DataWerx/datawerx-mesh/pkg/agent"
)

func main() {
	if err := agent.Run(agent.Options{}); err != nil {
		ctrl.Log.WithName("setup").Error(err, "fatal")
		os.Exit(1)
	}
}
