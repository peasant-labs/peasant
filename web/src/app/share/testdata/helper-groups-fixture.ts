/**
 * Strict typed loader for the mounted helper-group chooser corpus. The YAML is
 * untrusted test input: every field is validated at runtime, `requiredNames`
 * must equal the set of `role` names actually present, and the caller may pass
 * an independent expected inventory that must match both. No cast disables the
 * validation, and a mutation (empty manifest, removed/renamed role, invalid
 * status, unknown owner arm) is rejected instead of silently shrinking the
 * corpus.
 */

import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parse } from 'yaml';

export type HelperGroupSyncStatus = 'new' | 'updated' | 'synced' | 'held';
export const HELPER_GROUP_SYNC_STATUSES: readonly HelperGroupSyncStatus[] = ['new', 'updated', 'synced', 'held'];

export type HelperGroupOwnerStatus =
  | 'unknown'
  | 'resolved'
  | 'general_link_only'
  | 'known_unavailable'
  | 'inaccessible'
  | 'conflicting';
export const HELPER_GROUP_OWNER_STATUSES: readonly HelperGroupOwnerStatus[] = [
  'unknown',
  'resolved',
  'general_link_only',
  'known_unavailable',
  'inaccessible',
  'conflicting',
];

export interface FixtureGroup {
  groupId: string;
  helperThreadCount: number;
  memberScope: string;
}

export interface FixtureOwner {
  role: string;
  id: string;
  project: string;
  projectHash: string;
  startTime: string;
  preview: string;
  syncStatus: HelperGroupSyncStatus;
  groups: FixtureGroup[];
}

export interface FixtureMember {
  role: string;
  groupId: string;
  id: string;
  preview: string;
  turnCount: number;
  syncStatus: HelperGroupSyncStatus;
  helperGroups?: FixtureGroup[];
}

export interface FixtureContext {
  role: string;
  groupId: string;
  ownerStatus: HelperGroupOwnerStatus;
  helperGroups: FixtureGroup[];
}

export interface HelperGroupFixture {
  requiredNames: string[];
  paging: Record<string, number>;
  owners: FixtureOwner[];
  members: FixtureMember[];
  contexts: FixtureContext[];
  contextMembers: FixtureMember[];
}

function fail(where: string, reason: string): never {
  throw new Error(`mounted helper-group fixture is invalid at ${where}: ${reason}; restore the role or fix the field, then rerun the mounted chooser tests`);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function requireRecord(value: unknown, where: string): Record<string, unknown> {
  if (!isRecord(value)) fail(where, `expected an object, received ${JSON.stringify(value)}`);
  return value;
}

function requireArray(value: unknown, where: string): unknown[] {
  if (!Array.isArray(value)) fail(where, `expected an array, received ${JSON.stringify(value)}`);
  return value;
}

function requireString(value: unknown, where: string): string {
  if (typeof value !== 'string' || value.trim().length === 0) fail(where, `expected a non-empty string, received ${JSON.stringify(value)}`);
  return value;
}

function requireCount(value: unknown, where: string): number {
  if (typeof value !== 'number' || !Number.isInteger(value) || value < 0) fail(where, `expected a non-negative integer, received ${JSON.stringify(value)}`);
  return value;
}

function requireEnum<T extends string>(value: unknown, allowed: readonly T[], where: string): T {
  if (typeof value !== 'string' || !(allowed as readonly string[]).includes(value)) {
    fail(where, `expected one of ${allowed.join(', ')}, received ${JSON.stringify(value)}`);
  }
  return value as T;
}

function parseGroup(value: unknown, where: string): FixtureGroup {
  const record = requireRecord(value, where);
  return {
    groupId: requireString(record.groupId, `${where}.groupId`),
    helperThreadCount: requireCount(record.helperThreadCount, `${where}.helperThreadCount`),
    memberScope: requireString(record.memberScope, `${where}.memberScope`),
  };
}

function parseGroups(value: unknown, where: string): FixtureGroup[] {
  return requireArray(value, where).map((group, index) => parseGroup(group, `${where}[${index}]`));
}

function parseNestedGroups(record: Record<string, unknown>, where: string): FixtureGroup[] | undefined {
  if (record.helperGroups === undefined) return undefined;
  const groups = parseGroups(record.helperGroups, `${where}.helperGroups`);
  if (groups.length === 0) fail(`${where}.helperGroups`, 'a declared nested group list must name at least one group or be omitted');
  return groups;
}

function parseOwner(value: unknown, where: string): FixtureOwner {
  const record = requireRecord(value, where);
  return {
    role: requireString(record.role, `${where}.role`),
    id: requireString(record.id, `${where}.id`),
    project: requireString(record.project, `${where}.project`),
    projectHash: requireString(record.projectHash, `${where}.projectHash`),
    startTime: requireString(record.startTime, `${where}.startTime`),
    preview: requireString(record.preview, `${where}.preview`),
    syncStatus: requireEnum(record.syncStatus, HELPER_GROUP_SYNC_STATUSES, `${where}.syncStatus`),
    groups: parseGroups(record.groups, `${where}.groups`),
  };
}

function parseMember(value: unknown, where: string): FixtureMember {
  const record = requireRecord(value, where);
  const member: FixtureMember = {
    role: requireString(record.role, `${where}.role`),
    groupId: requireString(record.groupId, `${where}.groupId`),
    id: requireString(record.id, `${where}.id`),
    preview: requireString(record.preview, `${where}.preview`),
    turnCount: requireCount(record.turnCount, `${where}.turnCount`),
    syncStatus: requireEnum(record.syncStatus, HELPER_GROUP_SYNC_STATUSES, `${where}.syncStatus`),
  };
  const nested = parseNestedGroups(record, where);
  if (nested) member.helperGroups = nested;
  return member;
}

function parseContext(value: unknown, where: string): FixtureContext {
  const record = requireRecord(value, where);
  return {
    role: requireString(record.role, `${where}.role`),
    groupId: requireString(record.groupId, `${where}.groupId`),
    ownerStatus: requireEnum(record.ownerStatus, HELPER_GROUP_OWNER_STATUSES, `${where}.ownerStatus`),
    helperGroups: parseGroups(record.helperGroups, `${where}.helperGroups`),
  };
}

const ROOT_KEYS = ['contextMembers', 'contexts', 'members', 'owners', 'paging', 'requiredNames'] as const;

/**
 * Parse and fully validate the fixture. `expectedRoleNames` is an independent
 * inventory the caller owns; the document's own `requiredNames` must equal it
 * exactly (both directions), so neither can shrink without failing.
 */
export function loadHelperGroupFixture(source: string, expectedRoleNames: readonly string[]): HelperGroupFixture {
  const parsed: unknown = parse(source, { strict: true, uniqueKeys: true });
  const root = requireRecord(parsed, '<root>');
  const keys = Object.keys(root).slice().sort().join(',');
  if (keys !== [...ROOT_KEYS].slice().sort().join(',')) {
    fail('<root>', `expected exactly ${[...ROOT_KEYS].sort().join(', ')}, received ${keys}`);
  }

  const requiredNames = requireArray(root.requiredNames, 'requiredNames').map((name, index) => requireString(name, `requiredNames[${index}]`));
  if (new Set(requiredNames).size !== requiredNames.length) fail('requiredNames', 'role names must be unique');

  const owners = requireArray(root.owners, 'owners').map((owner, index) => parseOwner(owner, `owners[${index}]`));
  const members = requireArray(root.members, 'members').map((member, index) => parseMember(member, `members[${index}]`));
  const contexts = requireArray(root.contexts, 'contexts').map((context, index) => parseContext(context, `contexts[${index}]`));
  const contextMembers = requireArray(root.contextMembers, 'contextMembers').map((member, index) => parseMember(member, `contextMembers[${index}]`));

  const declaredRoles = [...owners, ...members, ...contexts, ...contextMembers].map((row) => row.role);
  if (new Set(declaredRoles).size !== declaredRoles.length) fail('<corpus>', 'every role must name exactly one row; a duplicated role hides a swapped row');
  const requiredSet = new Set(requiredNames);
  const declaredSet = new Set(declaredRoles);
  for (const role of requiredSet) if (!declaredSet.has(role)) fail('requiredNames', `required role ${role} is not present in the corpus`);
  for (const role of declaredSet) if (!requiredSet.has(role)) fail('<corpus>', `row role ${role} is not declared in requiredNames`);
  const expectedSet = new Set(expectedRoleNames);
  for (const role of expectedSet) if (!requiredSet.has(role)) fail('requiredNames', `expected role ${role} is not declared; the corpus shrank or renamed a role`);
  for (const role of requiredSet) if (!expectedSet.has(role)) fail('requiredNames', `declared role ${role} is not in the expected inventory`);

  const sessionIds = [...owners, ...members, ...contextMembers].map((row) => row.id);
  if (new Set(sessionIds).size !== sessionIds.length) fail('<corpus>', 'session ids must be unique across owners, members and context members');

  const allMemberRows = [...members, ...contextMembers];
  const groupIds = new Set(allMemberRows.map((member) => member.groupId));
  for (const owner of owners) {
    for (const group of owner.groups) {
      if (!groupIds.has(group.groupId)) fail(`owners[${owner.id}].groups`, `group ${group.groupId} names no member row`);
    }
  }
  for (const context of contexts) {
    for (const group of context.helperGroups) {
      if (!groupIds.has(group.groupId)) fail(`contexts[${context.groupId}].helperGroups`, `group ${group.groupId} names no member row`);
    }
  }
  for (const member of allMemberRows) {
    for (const group of member.helperGroups ?? []) {
      if (group.groupId === member.groupId) fail(`members[${member.id}].helperGroups`, `group ${group.groupId} cannot nest inside itself`);
      if (!groupIds.has(group.groupId)) fail(`members[${member.id}].helperGroups`, `nested group ${group.groupId} names no member row`);
    }
  }

  const pagingRecord = requireRecord(root.paging, 'paging');
  const paging: Record<string, number> = {};
  for (const [groupId, size] of Object.entries(pagingRecord)) {
    if (!groupIds.has(groupId)) fail(`paging.${groupId}`, 'paging can only name a group that has member rows');
    paging[groupId] = requireCount(size, `paging.${groupId}`);
  }

  return { requiredNames, paging, owners, members, contexts, contextMembers };
}

/** The mounted corpus's independently owned role inventory. */
export const HELPER_GROUP_REQUIRED_ROLES: readonly string[] = [
  'owner-with-helpers',
  'second-owner-with-helpers',
  'nested-helper-owner',
  'eligible-member',
  'ineligible-member',
  'other-member',
  'second-group-member',
  'page-two-member',
  'nested-owner-member',
  'nested-member',
  'helper-only-context',
  'context-member',
];

/** Read the checked-in corpus and validate it against the role inventory. */
export function readHelperGroupFixture(): HelperGroupFixture {
  const source = readFileSync(resolve(process.cwd(), 'src/app/share/testdata/mounted-share-helper-groups.yaml'), 'utf8');
  return loadHelperGroupFixture(source, HELPER_GROUP_REQUIRED_ROLES);
}
