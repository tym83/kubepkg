CONTROLLER_GEN ?= $(shell go env GOPATH)/bin/controller-gen

.PHONY: generate test build crds-for-group

generate:
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd

test:
	go test ./...

build:
	go build -o bin/ ./cmd/...

# CRDs for a platform that serves the types under its own group:
#   make crds-for-group GROUP=packages.example.com OUT=dist/crd
crds-for-group:
	@test -n "$(GROUP)" -a -n "$(OUT)" || { echo "set GROUP and OUT"; exit 1; }
	python3 hack/crds_for_group.py config/crd $(OUT) $(GROUP)
