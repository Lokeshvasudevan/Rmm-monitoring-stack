.PHONY: build test vet run targets
build:
	go build ./cmd/exporter
test:
	go test ./...
vet:
	go vet ./...
run:
	REDFISH_PASSWORD=$${REDFISH_PASSWORD:?set REDFISH_PASSWORD} go run ./cmd/exporter --config.file=configs/exporter.yml
targets:
	python3 generate_targets.py inventory.csv > prometheus/redfish_targets.yml
