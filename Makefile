SHELL := /bin/bash
PYTHON ?= python3
VALUES ?= deploy/openshift-values.yaml
NAMESPACE ?= pi-pocket
RELEASE ?= pi-pocket

.PHONY: test lint format dry-run install image portal-image
lint:
	helm lint charts/pi-pocket -f $(VALUES)
	cd portal && go vet ./...

format:
	gofmt -w $$(find portal -name '*.go')

test:
	$(PYTHON) tests/test_chart.py
	cd portal && go test -race ./...
	@if ls images/*.test.mjs >/dev/null 2>&1; then node --test images/*.test.mjs; fi

dry-run:
	helm template $(RELEASE) charts/pi-pocket -n $(NAMESPACE) -f $(VALUES) | kubectl apply --dry-run=server -n $(NAMESPACE) -f -

install:
	helm upgrade --install $(RELEASE) charts/pi-pocket --namespace $(NAMESPACE) --create-namespace -f $(VALUES) --wait --timeout 10m

image:
	podman build --pull=always -f images/Containerfile -t quay.io/cldmnky/pi-pocket:dev .

portal-image:
	podman build --pull=always -f portal/Containerfile -t quay.io/cldmnky/pi-pocket-portal:dev portal
