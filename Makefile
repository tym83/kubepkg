CONTROLLER_GEN ?= $(shell go env GOPATH)/bin/controller-gen

.PHONY: generate test build crds-for-group chart-crds docs screenshots

generate:
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd
	$(MAKE) chart-crds

test:
	go test ./...

build:
	go build -o bin/ ./cmd/...

# CRDs for a platform that serves the types under its own group:
#   make crds-for-group GROUP=packages.example.com OUT=dist/crd
crds-for-group:
	@test -n "$(GROUP)" -a -n "$(OUT)" || { echo "set GROUP and OUT"; exit 1; }
	python3 hack/crds_for_group.py config/crd $(OUT) $(GROUP)

# The chart ships the CRDs; keep its copy in step with config/crd.
chart-crds:
	rm -f charts/kubepkg/files/crds/*.yaml
	cp config/crd/*.yaml charts/kubepkg/files/crds/

# Reference pages generated from the commands and the CRDs.
docs:
	go run ./hack/gendocs config/crd docs/reference/cli.md docs/reference/api.md

# Terminal screenshots for the docs, drawn from the transcripts that
# hack/docs/capture.sh records on a real cluster.
SCREENSHOTS = plan install upgrade rollback trust init
screenshots:
	@for s in $(SCREENSHOTS); do python3 hack/docs/term2svg.py docs/examples/$$s.txt docs/img/$$s.svg "kubepkg — $$s"; done
