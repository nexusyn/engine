// Package migrations embed os arquivos SQL via go:embed.
//
// O `embed.FS` retornado por FS() é consumido pelo cmd/nexus/migrate.go via goose.
// Vantagem: migrations viram parte do binário — `nexus migrate up` funciona
// sem precisar do diretório migrations/ no disco.
package migrations

import "embed"

// FS contém todas as .sql desta pasta (incluindo as próprias migrations).
//
//go:embed *.sql
var FS embed.FS
