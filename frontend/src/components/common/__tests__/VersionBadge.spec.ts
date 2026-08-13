import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import VersionBadge from '../VersionBadge.vue'

const authStore = { isAdmin: true }
const appStore = {
  versionLoading: false,
  currentVersion: '0.1.175',
  latestVersion: '0.1.175',
  hasUpdate: false,
  releaseInfo: null,
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
