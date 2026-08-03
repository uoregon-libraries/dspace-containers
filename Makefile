TPL := tps-templates

.PHONY: tools base-template final-templates clean

tools: bin/htmlfmt bin/assemble

bin/htmlfmt: tools/htmlfmt/*.go tools/htmlfmt/go.mod
	mkdir -p bin
	go build -C tools/htmlfmt -o $(abspath bin/htmlfmt)

bin/assemble: tools/assemble/*.go tools/assemble/go.mod
	mkdir -p bin
	go build -C tools/assemble -o $(abspath bin/assemble)

# Reformat the raw SingleFile snapshot and extract its CSS to sb.css. The
# result still needs the manual cleanup described in tps-templates/README.md
# before final-templates can run.
base-template: bin/htmlfmt
	./bin/htmlfmt -raw $(TPL)/raw.html -output $(TPL)/base.html

# Build the deployable TPS templates from the cleaned base.html plus the
# per-page substitution files.
final-templates: bin/assemble
	./bin/assemble -dir $(TPL) -page challenge
	./bin/assemble -dir $(TPL) -page failed

clean:
	rm -f bin/htmlfmt bin/assemble $(TPL)/challenge.go.html $(TPL)/failed.go.html
