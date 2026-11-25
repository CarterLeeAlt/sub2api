import { createHash } from 'node:crypto'
import { existsSync, mkdirSync, readFileSync, realpathSync, writeFileSync, chmodSync, copyFileSync, symlinkSync } from 'node:fs'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

export const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const frontend = path.join(root, 'frontend')
const manifest = JSON.parse(readFileSync(path.join(frontend, 'package.json'), 'utf8'))
const nodeVersion = manifest.engines.node
export const pnpmVersion = manifest.packageManager.replace(/^pnpm@/, '')
const platform = process.platform === 'win32' ? 'win' : process.platform
export const nodePath = path.join(root, '.toolchains', `node-v${nodeVersion}-${platform}-${process.arch}`, process.platform === 'win32' ? 'node.exe' : 'bin/node')
const libc = process.platform === 'linux' && !process.report.getReport().header.glibcVersionRuntime ? '-musl' : ''
const pnpmTarget = `${process.platform}-${process.arch}${libc}`
export const pnpmPackage = `@pnpm/exe.${pnpmTarget}`
export const pnpmPath = path.join(root, '.toolchains', `pnpm-${pnpmVersion}-${pnpmTarget}`, 'package', process.platform === 'win32' ? 'pnpm.exe' : 'pnpm')
const shimDir = path.join(root, '.toolchains', 'frontend-bin')
const workspaceConfig = readFileSync(path.join(frontend, 'pnpm-workspace.yaml'), 'utf8')
const config = Object.fromEntries([...workspaceConfig.matchAll(/^(storeDir|cacheDir):\s*(\S+)\s*$/gm)].map(match => [match[1], match[2]]))
const locations = {
  pnpm_config_store_dir: path.resolve(frontend, config.storeDir),
  npm_config_cache: path.join(root, '.cache', 'npm'),
  pnpm_config_cache_dir: path.resolve(frontend, config.cacheDir),
  pnpm_config_state_dir: path.join(root, '.cache', 'frontend', 'state'),
  pnpm_config_virtual_store_dir: path.join(frontend, 'node_modules', '.pnpm'),
  pnpm_config_modules_dir: path.join(frontend, 'node_modules'),
  pnpm_config_lockfile_dir: frontend,
  pnpm_config_dir: frontend,
  pnpm_config_workspace_dir: frontend,
  pnpm_config_global_dir: path.join(root, '.toolchains', 'pnpm-home', 'global'),
  pnpm_config_global_bin_dir: shimDir,
  pnpm_config_config_dir: path.join(root, '.toolchains', 'pnpm-home', 'config'),
  PNPM_HOME: path.join(root, '.toolchains', 'pnpm-home'),
  COREPACK_HOME: path.join(root, '.toolchains', 'corepack-home'),
  XDG_CACHE_HOME: path.join(root, '.cache', 'frontend'),
  XDG_DATA_HOME: path.join(root, '.toolchains', 'pnpm-home', 'data'),
  XDG_STATE_HOME: path.join(root, '.cache', 'frontend', 'state'),
  npm_config_userconfig: path.join(root, '.cache', 'frontend', 'user.npmrc'),
}

function inside(base, target) {
  const relative = path.relative(base, target)
  return relative === '' || (!relative.startsWith(`..${path.sep}`) && relative !== '..' && !path.isAbsolute(relative))
}

export function assertWorkspacePath(target) {
  const resolved = path.resolve(target)
  if (!inside(root, resolved)) throw new Error(`Path outside repository: ${target}`)
  let existing = resolved
  while (!existsSync(existing)) existing = path.dirname(existing)
  if (!inside(realpathSync(root), realpathSync(existing))) throw new Error(`Path resolves outside repository: ${target}`)
  return resolved
}

export function validateEnvironment(source) {
  for (const [key, value] of Object.entries(source)) {
    const expectedKey = Object.keys(locations).find(item => item.toLowerCase() === key.toLowerCase().replaceAll('-', '_'))
    if (expectedKey && value) assertWorkspacePath(path.resolve(frontend, value))
  }
  for (const location of Object.values(locations)) assertWorkspacePath(location)
}

function validateRuntime() {
  assertWorkspacePath(process.execPath)
  if (process.version !== `v${nodeVersion}` || realpathSync(process.execPath) !== realpathSync(nodePath)) {
    throw new Error(`Use the repository Node ${nodeVersion}: ${nodePath}`)
  }
}

function validatePnpm() {
  assertWorkspacePath(pnpmPath)
  if (!existsSync(pnpmPath)) throw new Error('Local pnpm is missing. Run: pwsh -NoProfile -File tools/frontend.ps1 setup')
  const pkg = JSON.parse(readFileSync(path.join(path.dirname(pnpmPath), 'package.json'), 'utf8'))
  if (pkg.name !== pnpmPackage || pkg.version !== pnpmVersion) throw new Error(`Local pnpm must be ${pnpmVersion}`)
}

export function validateArguments(args) {
  const [command] = args
  if (!['install', 'run', 'exec', 'audit', 'add', 'update', 'remove', 'list', 'why', '--version'].includes(command)) {
    throw new Error('Allowed commands: setup, install, run, exec, audit, add, update, remove, list, why, --version')
  }
  for (const arg of args) {
    const option = arg.replace(/([a-z])([A-Z])/g, '$1-$2')
    if (/^(?:-g|-C|--global|--(?:config\.)?(?:dir|prefix|store-dir|cache-dir|state-dir|virtual-store-dir|modules-dir|lockfile-dir|workspace-dir|global-dir|global-bin-dir|config-dir|userconfig|pnpm-home-dir|pm-on-fail))(?:=|$)/i.test(option) || /^-C./.test(arg)) {
      throw new Error(`Location/toolchain override is not allowed: ${arg}`)
    }
  }
}

function command(file, args, options = {}) {
  const result = spawnSync(file, args, { cwd: frontend, stdio: 'inherit', windowsHide: true, ...options })
  if (result.error) throw result.error
  if (result.status !== 0) throw new Error(`${path.basename(file)} exited with ${result.status ?? result.signal}`)
  return result.stdout
}

function environment() {
  validateEnvironment(process.env)
  const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !Object.keys(locations).some(item => item.toLowerCase() === key.toLowerCase().replaceAll('-', '_')) && key.toLowerCase() !== 'path'))
  Object.assign(env, locations, {
    PATH: [path.dirname(nodePath), shimDir, process.env.PATH ?? process.env.Path ?? ''].join(path.delimiter),
    SUB2API_FRONTEND_ROOT: root,
    COREPACK_ENABLE_AUTO_PIN: '0',
    COREPACK_ENABLE_NETWORK: '0',
    pnpm_config_pm_on_fail: 'error',
    pnpm_config_modules_dir: 'node_modules',
  })
  return env
}

function prepareLocations() {
  for (const [key, value] of Object.entries(locations)) {
    if (key.endsWith('config')) {
      mkdirSync(path.dirname(value), { recursive: true })
      if (!existsSync(value)) writeFileSync(value, '')
    } else mkdirSync(value, { recursive: true })
  }
  assertWorkspacePath(shimDir)
  mkdirSync(shimDir, { recursive: true })
  const entry = fileURLToPath(import.meta.url)
  if (process.platform === 'win32') {
    writeFileSync(path.join(shimDir, 'pnpm.cmd'), `@echo off\r\n"${nodePath}" "${entry}" %*\r\n`)
  } else {
    const quote = text => `'${text.replaceAll("'", "'\\''")}'`
    const shim = path.join(shimDir, 'pnpm')
    writeFileSync(shim, `#!/bin/sh\nexec ${quote(nodePath)} ${quote(entry)} "$@"\n`)
    chmodSync(shim, 0o755)
  }
}

function setup(env) {
  if (!existsSync(pnpmPath)) {
    const downloads = assertWorkspacePath(path.join(root, '.toolchains', 'downloads'))
    mkdirSync(downloads, { recursive: true })
    const curl = process.platform === 'win32' ? 'curl.exe' : 'curl'
    const metadata = JSON.parse(command(curl, ['--fail', '--silent', '--show-error', '--location', `https://registry.npmjs.org/${pnpmPackage}/${pnpmVersion}`], { env, encoding: 'utf8', stdio: ['ignore', 'pipe', 'inherit'] }))
    const archive = path.join(downloads, `pnpm-${pnpmVersion}-${pnpmTarget}.tgz`)
    command(curl, ['--fail', '--show-error', '--location', '--output', archive, metadata.dist.tarball], { env })
    const [algorithm, expected] = metadata.dist.integrity.split('-')
    if (createHash(algorithm).update(readFileSync(archive)).digest('base64') !== expected) throw new Error('pnpm archive integrity mismatch')
    const destination = assertWorkspacePath(path.resolve(pnpmPath, '../..'))
    mkdirSync(destination, { recursive: true })
    const tar = process.platform === 'win32' ? path.join(process.env.SystemRoot, 'System32', 'tar.exe') : 'tar'
    command(tar, ['-xzf', archive, '-C', destination], { env })
  }
  validatePnpm()
  prepareLocations()
  console.log(`Ready: Node ${nodeVersion}, pnpm ${pnpmVersion}; store ${locations.pnpm_config_store_dir}`)
}

function guard() {
  validatePnpm()
  validateEnvironment(process.env)
  if (process.env.SUB2API_FRONTEND_ROOT !== root || !process.env.npm_execpath || realpathSync(process.env.npm_execpath) !== realpathSync(pnpmPath)) {
    throw new Error('Use the isolated entry: pwsh -NoProfile -File tools/frontend.ps1 <command>')
  }
}

export function main(args) {
  // Container images supply the pinned runtime; copy it into the same local layout.
  if (args[0] === 'provision-node') {
    if (process.version !== `v${nodeVersion}`) throw new Error(`Node ${nodeVersion} is required`)
    assertWorkspacePath(nodePath)
    mkdirSync(path.dirname(nodePath), { recursive: true })
    if (!existsSync(nodePath)) copyFileSync(process.execPath, nodePath)
    if (process.platform !== 'win32') {
      assertWorkspacePath(shimDir)
      mkdirSync(shimDir, { recursive: true })
      const link = path.join(shimDir, 'node')
      if (!existsSync(link)) symlinkSync(nodePath, link)
      if (realpathSync(link) !== realpathSync(nodePath)) throw new Error('Unexpected local Node link')
    }
    return
  }
  validateRuntime()
  if (args[0] === 'guard') return guard()
  if (args[0] === 'build') {
    guard()
    for (const [bin, ...rest] of [
      ['vitest/vitest.mjs', 'run', 'src/i18n/__tests__/localeKeyCompleteness.spec.ts'],
      ['vue-tsc/bin/vue-tsc.js', '-b'],
      ['vite/bin/vite.js', 'build'],
    ]) command(nodePath, [path.join(frontend, 'node_modules', bin), ...rest])
    return
  }
  const env = environment()
  if (args[0] === 'setup') return setup(env)
  validatePnpm()
  validateArguments(args)
  prepareLocations()
  command(pnpmPath, args, { env })
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try { main(process.argv.slice(2)) } catch (error) {
    console.error(`Frontend isolation: ${error.message}`)
    process.exitCode = 1
  }
}
