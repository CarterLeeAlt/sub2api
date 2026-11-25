import assert from 'node:assert/strict'
import { copyFileSync, existsSync, linkSync, mkdirSync, mkdtempSync, readFileSync, symlinkSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import test from 'node:test'
import { nodePath, pnpmPath, pnpmVersion, pnpmPackage, root } from './frontend.mjs'

const temporary = path.join(root, '.tmp')
mkdirSync(temporary, { recursive: true })
const work = mkdtempSync(path.join(temporary, 'frontend-isolation-'))
const entry = path.join(root, 'tools', 'frontend.mjs')
const fakeBin = path.join(work, 'fake-bin')
mkdirSync(fakeBin)
const marker = path.join(work, 'global-tool-called')
for (const name of ['node', 'pnpm']) {
  if (process.platform === 'win32') writeFileSync(path.join(fakeBin, `${name}.cmd`), `@echo off\r\necho unexpected>"${marker}"\r\nexit /b 91\r\n`)
  else writeFileSync(path.join(fakeBin, name), `#!/bin/sh\nprintf unexpected > '${marker}'\nexit 91\n`, { mode: 0o755 })
}
const baseEnv = Object.fromEntries(Object.entries(process.env).filter(([key]) => key.toLowerCase() !== 'path'))
const poisonedEnv = { ...baseEnv, PATH: `${fakeBin}${path.delimiter}${process.env.PATH ?? process.env.Path ?? ''}` }
function run(args, env = poisonedEnv, script = entry, runtime = nodePath) {
  return spawnSync(runtime, [script, ...args], { cwd: root, env, encoding: 'utf8', windowsHide: true, timeout: 180_000, maxBuffer: 8 * 1024 * 1024 })
}
function failure(result, pattern) {
  assert.equal(result.error, undefined)
  assert.notEqual(result.status, 0)
  assert.match(result.stderr + result.stdout, pattern)
  assert.equal(existsSync(marker), false)
}

test('nested pnpm and node resolve locally even with a poisoned PATH', () => {
  const nested = run(['exec', 'pnpm', '--version'])
  assert.equal(nested.status, 0, nested.stderr)
  assert.equal(nested.stdout.trim(), pnpmVersion)
  const runtime = run(['exec', 'node', '-p', 'process.execPath'])
  assert.equal(runtime.status, 0, runtime.stderr)
  assert.equal(runtime.stdout.trim(), nodePath)
  assert.equal(existsSync(marker), false)
})

test('external stores and global installation are rejected before pnpm starts', () => {
  const outside = path.join(path.parse(root).root, 'sub2api-forbidden-store')
  assert.equal(existsSync(outside), false)
  for (const key of ['pnpm_config_store_dir', 'NPM_CONFIG_CACHE', 'pnpm_config_state_dir', 'pnpm_config_virtual_store_dir', 'pnpm_config_modules_dir', 'pnpm_config_lockfile_dir', 'pnpm_config_config_dir', 'PNPM_HOME']) {
    failure(run(['--version'], { ...poisonedEnv, [key]: outside }), /Path outside repository/)
  }
  for (const args of [['install', '--store-dir', outside], ['add', '-g', 'pnpm'], ['install', '--config.cache-dir=' + outside], ['install', '--storeDir=' + outside], ['install', '--modules-dir=' + outside]]) {
    failure(run(args), /override is not allowed/)
  }
  assert.equal(existsSync(outside), false)
})

test('directory links cannot redirect the store outside the repository', () => {
  const link = path.join(work, 'external-link')
  symlinkSync(path.parse(root).root, link, process.platform === 'win32' ? 'junction' : 'dir')
  failure(run(['--version'], { ...poisonedEnv, pnpm_config_store_dir: path.join(link, 'forbidden-store') }), /resolves outside repository/)
})

test('direct package-manager invocation cannot bypass the lifecycle guard', () => {
  const env = { ...poisonedEnv, PATH: `${path.dirname(nodePath)}${path.delimiter}${poisonedEnv.PATH}` }
  delete env.SUB2API_FRONTEND_ROOT
  const result = spawnSync(pnpmPath, ['--dir', path.join(root, 'frontend'), 'run', 'check:i18n'], {
    env, encoding: 'utf8', windowsHide: true, timeout: 30_000,
  })
  failure(result, /Use the isolated entry/)
})

test('missing tools and incorrect pnpm versions fail without using global tools', () => {
  const fixture = path.join(work, 'missing-tools')
  mkdirSync(path.join(fixture, 'tools'), { recursive: true })
  mkdirSync(path.join(fixture, 'frontend'))
  for (const file of ['frontend/package.json', 'frontend/.npmrc', 'frontend/pnpm-workspace.yaml', 'tools/frontend.mjs', 'tools/frontend.ps1']) {
    copyFileSync(path.join(root, file), path.join(fixture, file))
  }
  const missingNode = spawnSync('pwsh', ['-NoProfile', '-File', path.join(fixture, 'tools/frontend.ps1'), '--version'], {
    env: { ...baseEnv, PATH: process.env.PATH ?? process.env.Path }, encoding: 'utf8', windowsHide: true, timeout: 30_000,
  })
  failure(missingNode, /Local Node is missing/)
  const fixtureNode = path.join(fixture, path.relative(root, nodePath))
  mkdirSync(path.dirname(fixtureNode), { recursive: true })
  linkSync(nodePath, fixtureNode)
  const clean = Object.fromEntries(Object.entries(poisonedEnv).filter(([key]) => !/^(npm_config_|pnpm_config_|pnpm_home|corepack_home|xdg_|sub2api_)/i.test(key)))
  failure(run(['--version'], clean, path.join(fixture, 'tools/frontend.mjs'), fixtureNode), /Local pnpm is missing/)
  const fixturePnpm = path.join(fixture, path.relative(root, pnpmPath))
  mkdirSync(path.dirname(fixturePnpm), { recursive: true })
  writeFileSync(fixturePnpm, 'throw new Error("must not execute")')
  writeFileSync(path.join(path.dirname(fixturePnpm), 'package.json'), JSON.stringify({ name: pnpmPackage, version: '99.0.0' }))
  failure(run(['--version'], clean, path.join(fixture, 'tools/frontend.mjs'), fixtureNode), new RegExp(`Local pnpm must be ${pnpmVersion.replaceAll('.', '\\.')}`))
  const packageFile = path.join(fixture, 'frontend/package.json')
  const manifest = JSON.parse(readFileSync(packageFile, 'utf8'))
  manifest.engines.node = '0.0.0'
  writeFileSync(packageFile, JSON.stringify(manifest))
  failure(run(['--version'], clean, path.join(fixture, 'tools/frontend.mjs'), fixtureNode), /Use the repository Node 0.0.0/)
})

test('offline install and production build stay local with a poisoned PATH', () => {
  const driveStore = path.join(path.parse(root).root, '.pnpm-store')
  const existed = existsSync(driveStore)
  const installed = run(['install', '--frozen-lockfile', '--offline'])
  assert.equal(installed.status, 0, installed.stdout + installed.stderr)
  const result = run(['run', 'build'])
  assert.equal(result.status, 0, result.stdout + result.stderr)
  assert.equal(existsSync(marker), false)
  assert.equal(existsSync(driveStore), existed)
})
