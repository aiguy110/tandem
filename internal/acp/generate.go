package acp

// The schema is supplied by the exact SDK package pinned in runtime/package-lock.json.
//go:generate go run ./cmd/schema-gen -schema ../../runtime/node_modules/@agentclientprotocol/sdk/schema/schema.json -version 1.2.1 -sha256 8bdfd8347ce8bd2c8620b71bfd5460625f91c7db47a51268bb42b67014ea5b1f -out types_generated.go

// ACPWireProtocolVersion is ACP's negotiated wire version. It is intentionally not
// derived from, or compared numerically with, SchemaPackageVersion.
const ACPWireProtocolVersion = 1
