#!/usr/bin/env node
// Token guard for the console design system.
//
// Two failures it catches, both of which are invisible in a build:
//   1. A reference to a retired token name. These resolve to nothing, so the
//      declaration is silently dropped and the element inherits — a bug that
//      only shows up as "this border is missing" three phases later.
//   2. A reference to a token nobody defined. Same symptom, opposite cause.
//
// Run it after touching theme.css or any component's styles:
//   npm run check:tokens          # scan the real tree
//   npm run check:tokens -- --selftest   # prove the guard can still fail
//
// Policy: comments are scanned too. A retired name in a comment is how the old
// vocabulary creeps back into the next copy-paste, so it is treated the same as
// code. The mapping stays in docs/UI_REDESIGN_SPEC.md §2.7.
//
// Scope note: --cols* , --table-min and --cfg-min are component-local
// variables, exempt from the definition check.

import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'

const ROOT = new URL('..', import.meta.url).pathname.replace(/^\/([A-Za-z]:)/, '$1')
const SRC = join(ROOT, 'src')
const THEME = join(SRC, 'theme.css')

// Names retired in P1. This list only grows.
const RETIRED = [
  '--background',
  '--foreground',
  '--card',
  '--card-foreground',
  '--card-radius',
  '--card-padding',
  '--popover',
  '--popover-foreground',
  '--primary',
  '--primary-foreground',
  '--primary-light',
  '--primary-dark',
  '--primary-medium',
  '--primary-subtle',
  '--primary-strong',
  '--glow-orange',
  '--secondary',
  '--secondary-foreground',
  '--muted',
  '--muted-foreground',
  '--accent',
  '--accent-foreground',
  '--success',
  '--success-bg',
  '--warning',
  '--warning-bg',
  '--destructive',
  '--destructive-bg',
  '--info-bg',
  '--border-subtle',
  '--border-light',
  '--input',
  '--input-radius',
  '--input-height',
  '--input-padding',
  '--ring',
  '--surface-hover',
  '--surface-active',
  '--overlay-bg',
  '--backdrop-color',
  '--nav-bg',
  '--nav-bg-scrolled',
  '--nav-blur',
  '--glass-bg',
  '--glass-blur',
  '--radius',
  '--radius-sm',
  '--radius-lg',
  '--radius-xl',
  '--button-radius',
  '--button-height',
  '--button-height-dense',
  '--shadow-sm',
  '--shadow-md',
  '--shadow-lg',
  '--shadow-modal',
  '--text-muted',
  '--text-body',
  '--text-2xs',
  '--text-xs',
  '--text-sm',
  '--text-caption',
  '--text-base',
  '--text-lg',
  '--text-xl',
  '--text-2xl',
  '--text-3xl',
  '--font-family-primary',
  '--font-family-mono',
  '--font-regular',
  '--font-medium',
  '--font-semibold',
  '--tracking-tight',
  '--tracking-normal',
  '--tracking-wide',
  '--tracking-caps',
  '--transition-fast',
  '--transition-normal',
  '--transition-smooth',
  '--transition-ios',
  '--ease-out-soft',
  '--z-base',
  '--z-relative',
  '--z-dropdown',
  '--z-sticky-header',
  '--z-modal',
  '--z-modal-backdrop',
  '--page-max-width',
  '--page-padding-x',
  '--page-padding-x-mobile',
  '--page-top',
  '--page-bottom',
  '--container',
  '--idle-bg',
  '--ok-bg',
  '--warn-bg',
  '--bad',
  '--bad-bg',
]

// Component-local geometry, not design vocabulary: --cols* and --table-min are
// set inline from markup, --cfg-min is the height of the settings viewport
// remainder (declared on the page's own root because the loader and the panel
// must measure themselves with the same number).
const LOCAL = /^--(cols|table-min|cfg-min)/

function walk(dir) {
  const out = []
  for (const name of readdirSync(dir)) {
    const full = join(dir, name)
    if (statSync(full).isDirectory()) out.push(...walk(full))
    else if (/\.(svelte|js|css)$/.test(name)) out.push(full)
  }
  return out
}

// scan takes plain {name, text} records so the selftest can feed it fixtures
// without touching the tree.
function scan(records, defined) {
  const retired = []
  const undefinedNames = []
  for (const { name, text } of records) {
    const lines = text.split('\n')
    lines.forEach((line, i) => {
      for (const m of line.matchAll(/var\((--[A-Za-z0-9-]+)/g)) {
        const token = m[1]
        if (RETIRED.includes(token)) retired.push(`${name}:${i + 1}  ${token}`)
        else if (!defined.has(token) && !LOCAL.test(token)) {
          undefinedNames.push(`${name}:${i + 1}  ${token}`)
        }
      }
    })
  }
  return { retired, undefinedNames }
}

const themeText = readFileSync(THEME, 'utf8')
const defined = new Set([...themeText.matchAll(/^\s*(--[A-Za-z0-9-]+)\s*:/gm)].map((m) => m[1]))

if (process.argv.includes('--selftest')) {
  const cases = [
    {
      name: 'retired name in a rule',
      text: '.x { color: var(--primary); }',
      want: 'retired',
    },
    {
      name: 'retired name inside a comment',
      text: '/* was var(--muted-foreground) */',
      want: 'retired',
    },
    {
      name: 'name nobody defined',
      text: '.x { color: var(--does-not-exist); }',
      want: 'undefined',
    },
    {
      name: 'clean file with local grid variables',
      text: '.x { color: var(--text-2); grid-template-columns: var(--cols-x); min-width: var(--table-min); }',
      want: 'none',
    },
  ]

  let failures = 0
  for (const c of cases) {
    const result = scan([{ name: c.name, text: c.text }], defined)
    const got = result.retired.length ? 'retired' : result.undefinedNames.length ? 'undefined' : 'none'
    if (got !== c.want) {
      failures += 1
      console.log(`SELFTEST_FAIL  case="${c.name}" want=${c.want} got=${got}`)
    } else {
      console.log(`SELFTEST_OK    case="${c.name}" -> ${got}`)
    }
  }

  // The two categories must stay distinguishable: a guard that reports every
  // miss as "retired" would hide a typo behind a refactor message.
  const mixed = scan(
    [{ name: 'mixed', text: 'a { color: var(--primary) } b { color: var(--nope-xyz) }' }],
    defined,
  )
  if (!mixed.retired.length || !mixed.undefinedNames.length) {
    failures += 1
    console.log('SELFTEST_FAIL  case="mixed categories" categories are not separated')
  } else {
    console.log('SELFTEST_OK    case="mixed categories" -> both reported')
  }

  if (failures) {
    console.log(`RESULT=FAIL  ${failures} selftest case(s) failed`)
    process.exit(1)
  }
  console.log('RESULT=OK  guard catches violations and stays quiet on clean input')
  process.exit(0)
}

const records = walk(SRC).map((file) => ({
  name: relative(ROOT, file).replace(/\\/g, '/'),
  text: readFileSync(file, 'utf8'),
}))
const { retired, undefinedNames } = scan(records, defined)

let failed = false
if (retired.length) {
  failed = true
  console.error(`Retired token names in use (${retired.length}):`)
  for (const hit of retired) console.error('  ' + hit)
}
if (undefinedNames.length) {
  failed = true
  console.error(`Tokens referenced but never defined (${undefinedNames.length}):`)
  for (const hit of undefinedNames) console.error('  ' + hit)
}

if (failed) {
  console.error('\nToken guard failed. See docs/UI_REDESIGN_SPEC.md §2 for the registry.')
  process.exit(1)
}

console.log(`Token guard passed: ${defined.size} defined, ${records.length} files scanned.`)
