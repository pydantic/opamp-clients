module github.com/pydantic/opamp-clients/conformance/drivers/go-core

go 1.25.0

require (
	github.com/open-telemetry/opamp-go v0.24.0
	github.com/pydantic/opamp-clients/go v0.0.0
)

require (
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/tetratelabs/wazero v1.12.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace github.com/pydantic/opamp-clients/go => ../../../go
