package store

// Retirement keeps annotation values and targets available to historical readers.
const migrationV54 = `ALTER TABLE annotations ADD COLUMN retired_at INTEGER;`
