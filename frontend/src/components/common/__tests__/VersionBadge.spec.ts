import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import VersionBadge from '../VersionBadge.vue'
import type { ReleaseInfo } from '@/api/admin/system'

const authStore = { isAdmin: true }
const appStore = {
  versionLoading: false,
  currentVersion: '0.1.175',
  latestVersion: '0.1.175',
  hasUpdate: false,
  versionWarning: '',
  versionWarningCode: '',
  releaseInfo: null as ReleaseInfo | null,
  fetchVersion: vi.fn()
}

vi.mock('@/stores', () => ({
  useAuthStore: () => authStore,
  useAppStore: () => appStore
}))

vi.mock('@/utils/buildIdentity', () => ({
  formatBuildIdentity: () => 'sha-075c6ea'
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key })
}))

const mountBadge = () => mount(VersionBadge, {
  global: {
    stubs: { Icon: true }
  }
})

describe('VersionBadge', () => {
  beforeEach(() => {
    authStore.isAdmin = true
    appStore.currentVersion = '0.1.175'
    appStore.latestVersion = '0.1.175'
    appStore.hasUpdate = false
    appStore.releaseInfo = null
    appStore.versionWarning = ''
    appStore.versionWarningCode = ''
    appStore.fetchVersion.mockClear()
  })

  it('shows only the release version in the collapsed admin badge', () => {
    const wrapper = mountBadge()
    const button = wrapper.get('button')

    expect(button.text()).toContain('v0.1.175')
    expect(button.text()).not.toContain('sha-075c6ea')
  })

  it('keeps the build identity in the expanded details', async () => {
    const wrapper = mountBadge()
    await wrapper.get('button').trigger('click')

    expect(wrapper.text()).toContain('v0.1.175')
    expect(wrapper.text()).toContain('sha-075c6ea')
  })

  it('identifies a missing upstream release and links to upstream', async () => {
    appStore.versionWarning = 'No release has been published for Wei-Shaw/sub2api.'
    appStore.versionWarningCode = 'no_release'
    const wrapper = mountBadge()
    await wrapper.get('button').trigger('click')
    expect(wrapper.text()).toContain('version.noRelease')
    expect(wrapper.text()).not.toContain('version.latestVersion')
    expect(wrapper.get('a').attributes('href')).toBe('https://github.com/Wei-Shaw/sub2api')
    expect(wrapper.text()).not.toContain('version.viewChangelog')
    wrapper.unmount()
  })

  it('shows the latest upstream version even when there is no newer release', async () => {
    const wrapper = mountBadge()
    await wrapper.get('button').trigger('click')
    expect(wrapper.text()).toContain('version.latestVersion: v0.1.175')
    expect(wrapper.text()).not.toContain('version.updateAvailable')
    wrapper.unmount()
  })

  it('highlights an upstream update and links to its release notes', async () => {
    appStore.latestVersion = '0.2.22'
    appStore.hasUpdate = true
    appStore.releaseInfo = { name: 'v0.2.22', body: '', published_at: '', html_url: 'https://github.com/Wei-Shaw/sub2api/releases/tag/v0.2.22' }
    const wrapper = mountBadge()
    await wrapper.get('button').trigger('click')
    expect(wrapper.get('button').classes()).toContain('bg-amber-100')
    expect(wrapper.text()).toContain('version.latestVersion: v0.2.22')
    expect(wrapper.text()).toContain('version.updateAvailable')
    expect(wrapper.get('a').attributes('href')).toBe(appStore.releaseInfo.html_url)
    wrapper.unmount()
  })

  it.each([false, true])('reports a check failure with cached release=%s and retains the reason', async cached => {
    appStore.versionWarning = 'GitHub API returned 503'
    appStore.versionWarningCode = 'check_failed'
    if (cached) appStore.releaseInfo = { name: 'v0.1.175', body: '', published_at: '', html_url: 'https://github.com/Wei-Shaw/sub2api/releases/tag/v0.1.175' }
    const wrapper = mountBadge()
    await wrapper.get('button').trigger('click')
    const status = wrapper.get('p')
    expect(status.text()).toBe(cached ? 'version.cachedResult' : 'version.checkFailed')
    expect(status.attributes('title')).toBe('GitHub API returned 503')
    expect(wrapper.text()).not.toContain('version.latestVersion')
    wrapper.unmount()
  })

  it('also hides the build identity for non-admin users', () => {
    authStore.isAdmin = false
    const wrapper = mount(VersionBadge, {
      props: { version: '0.1.175' },
      global: { stubs: { Icon: true } }
    })

    expect(wrapper.text()).toBe('v0.1.175')
    expect(wrapper.text()).not.toContain('sha-075c6ea')
  })
})
