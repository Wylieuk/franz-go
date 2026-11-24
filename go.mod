module github.com/twmb/franz-go

go 1.25.0

require (
	github.com/klauspost/compress v1.18.7
	github.com/pierrec/lz4/v4 v4.1.21
	github.com/twmb/franz-go/pkg/kmsg v1.9.0
	golang.org/x/crypto v0.52.0
)

retract v1.11.4 // This version is actually a breaking change and requires a major version change.
