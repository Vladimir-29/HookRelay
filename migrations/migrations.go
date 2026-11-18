// Package migrations содержит SQL-миграции, встроенные в бинарник.
// Применяются командой hookrelay-migrate (internal/migrate).
package migrations

import "embed"

// FS — все SQL-файлы миграций. Имена файлов определяют порядок
// применения: 001_..., 002_..., ...
//
//go:embed *.sql
var FS embed.FS
