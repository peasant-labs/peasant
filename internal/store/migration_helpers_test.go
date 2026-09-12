package store

import "zombiezen.com/go/sqlite/sqlitemigration"

// frozenSchema returns the schema as it shipped after exactly n migrations, so
// a migration test can seed the predecessor state through raw SQL before the
// migration under test runs. Shipped migrations stay immutable, so a frozen
// prefix is a stable description of an installed database.
func frozenSchema(n int) sqlitemigration.Schema {
	return sqlitemigration.Schema{Migrations: dbSchema.Migrations[:n], MigrationOptions: dbSchema.MigrationOptions[:n]}
}
