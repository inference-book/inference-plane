module github.com/inference-book/inference-plane

go 1.26.2

require (
	github.com/panyam/servicekit v0.0.4
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/gorilla/websocket v1.5.0 // indirect
	github.com/panyam/gocurrent v0.1.0 // indirect
	github.com/panyam/goutils v0.1.8 // indirect
	golang.org/x/sys v0.38.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251029180050-ab9386a59fda // indirect
	google.golang.org/grpc v1.78.0 // indirect
	google.golang.org/protobuf v1.36.10 // indirect
)

replace github.com/panyam/servicekit => ../../../newstack/servicekit/master
