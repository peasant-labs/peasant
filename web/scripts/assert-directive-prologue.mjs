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
  const functionBodies = new WeakMap()
  function visit(node, parent) {
    if (!node || typeof node !== 'object') return
    if ((node.type === 'FunctionDeclaration' || node.type === 'FunctionExpression' ||
         node.type === 'ArrowFunctionExpression') && node.body?.type === 'BlockStatement') {
      functionBodies.set(node.body, node)
    }
    if (node.type === 'ExpressionStatement' && node.expression?.type === 'Literal' &&
        (node.expression.value === 'use client' || node.expression.value === 'use server')) {
      const value = node.expression.value
      const validModule = parent === ast && node === ast.body[0] && node.directive === value
      const owner = functionBodies.get(parent)
      const validAction = value === 'use server' && owner?.async && node.directive === value
      if (!validModule && !validAction) {
        throw new Error(`${path}: "${value}" must be the first statement of a module or a valid async function directive; remove parentheses and escapes, and move it before other statements`)
      }
    }
    for (const value of Object.values(node)) {
      if (Array.isArray(value)) value.forEach((child) => visit(child, node))
      else if (value && typeof value === 'object') visit(value, node)
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
