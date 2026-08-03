import YAML from 'yaml';

export function parseStrictYAML(source: string, label: string): unknown {
  if ((source.match(/^---\s*$/gm) ?? []).length > 0) {
    throw new Error(`${label} must contain exactly one YAML document`);
  }
  const document = YAML.parseDocument(source, { strict: true, uniqueKeys: true });
  if (document.errors.length > 0) {
    throw new Error(`${label} is invalid YAML: ${document.errors.map((error) => error.message).join('; ')}`);
  }
  return document.toJS();
}

export function requireRecord(value: unknown, path: string): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(`${path} must be an object`);
  }
  return value as Record<string, unknown>;
}

export function requireExactFields(value: Record<string, unknown>, allowed: readonly string[], path: string): void {
  const unknown = Object.keys(value).filter((key) => !allowed.includes(key));
  if (unknown.length > 0) throw new Error(`${path} has unknown fields: ${unknown.join(', ')}`);
}

export function requireExactRequiredFields(value: Record<string, unknown>, fields: readonly string[], path: string): void {
  requireExactFields(value, fields, path);
  const missing = fields.filter((key) => !(key in value));
  if (missing.length > 0) throw new Error(`${path} is missing required fields: ${missing.join(', ')}`);
}

export function requireUniqueNames(rows: readonly Record<string, unknown>[], path: string): void {
  const names = new Set<string>();
  rows.forEach((row, index) => {
    if (typeof row.name !== 'string' || row.name === '') throw new Error(`${path}[${index}].name must be a non-empty string`);
    if (names.has(row.name)) throw new Error(`${path} duplicates name ${JSON.stringify(row.name)}`);
    names.add(row.name);
  });
}
