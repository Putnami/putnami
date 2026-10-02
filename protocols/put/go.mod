module go.putnami.dev/protocol/put

go 1.25.7

require (
	go.putnami.dev/protocol/diagnostic v0.0.0
	go.putnami.dev/protocol/distribution v0.0.0
)

replace (
	go.putnami.dev/protocol/diagnostic => ../diagnostic
	go.putnami.dev/protocol/distribution => ../distribution
)
