// Command test-watcher is a Code Foundry plugin that runs a project's tests in a
// thread's worktree whenever its files change, shows the result in a Tests side-panel
// tab (status, failing tests, live output), flags the thread while tests fail, and
// sends failures to Claude. plugin.json beside it declares its commands and surface.
package main

import (
	"fmt"
	"os"

	"github.com/alexwaumann/code-foundry/pluginsdk"
)

func main() {
	if err := pluginsdk.Serve(plugin()); err != nil {
		fmt.Fprintln(os.Stderr, "test-watcher:", err)
		os.Exit(1)
	}
}

// plugin serves everything plugin.json declares: its commands by name and its surfaces
// by id.
func plugin() pluginsdk.Plugin {
	return newApp(defaultConfig()).plugin()
}
