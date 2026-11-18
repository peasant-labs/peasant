import { readdir, readFile } from 'node:fs/promises'
import { join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import tseslint from 'typescript-eslint'

export function checkSource(source, path) {
  const { ast } = tseslint.parser.parseForESLint(source, {
    filePath: path,
    sourceType: 'module',
    ecmaFeatures: { jsx: true },
  })
  const first = ast.body[0]
  function visit(node) {
    if (!node || typeof node !== 'object') return
    if (node.type === 'ExpressionStatement' && node.expression?.type === 'Literal' &&
        (node.expression.value === 'use client' || node.expression.value === 'use server') &&
        node !== first) {
      throw new Error(`${path}: "${node.expression.value}" must be the first statement; move it before imports and other statements`)
    }
    for (const value of Object.values(node)) {
      if (Array.isArray(value)) value.forEach(visit)
      else if (value && typeof value === 'object') visit(value)
    }
  }
  visit(ast)
}

export async function checkDirectory(directory) {
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const path = join(directory, entry.name)
    if (entry.isDirectory()) await checkDirectory(path)
    else if (entry.isFile() && /\.(?:js|jsx|mjs|cjs|ts|tsx|mts|cts)$/.test(entry.name)) {
      checkSource(await readFile(path, 'utf8'), path)
    }
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  await checkDirectory(fileURLToPath(new URL('../src/', import.meta.url)))
}
