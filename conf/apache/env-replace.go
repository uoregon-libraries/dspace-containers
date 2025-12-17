// env-replace.go is like envsubst but (a) slightly more portable, (b) 100%
// written by AI, and (c) pukes out "<no value>" when a key isn't found so we
// can tell if there's a missing env var more easily.
//
// Input can be from a file or stdin. The format of env vars must be
// "{{.VAR}}". This isn't bash, people.

package main

import (
	"flag"
	"io"
	"os"
	"strings"
	"text/template"
)

func main() {
	// 1. Setup flags
	filePath := flag.String("f", "", "Path to the template file (default: read from stdin)")
	flag.Parse()

	// 2. Load Environment Variables into a map
	envMap := make(map[string]string)
	for _, e := range os.Environ() {
		pair := strings.SplitN(e, "=", 2)
		if len(pair) == 2 {
			envMap[pair[0]] = pair[1]
		}
	}

	// 3. Determine the input source
	var input []byte
	var err error

	if *filePath != "" {
		input, err = os.ReadFile(*filePath)
	} else {
		input, err = io.ReadAll(os.Stdin)
	}

	if err != nil {
		os.Stderr.WriteString("Error reading input: " + err.Error() + "\n")
		os.Exit(1)
	}

	// 4. Parse and Execute
	// We use "." in the template to access the map keys,
	// but we can map the root to the envMap directly.
	tmpl, err := template.New("env").Parse(string(input))
	if err != nil {
		os.Stderr.WriteString("Error parsing template: " + err.Error() + "\n")
		os.Exit(1)
	}

	// Execute against the envMap
	if err := tmpl.Execute(os.Stdout, envMap); err != nil {
		os.Stderr.WriteString("Error executing template: " + err.Error() + "\n")
		os.Exit(1)
	}
}
