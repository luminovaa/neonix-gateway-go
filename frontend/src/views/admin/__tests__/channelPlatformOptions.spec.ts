import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { CONCRETE_PLATFORM_OPTIONS } from '@/constants/platforms'

describe('Composite channel platform options', () => {
  it('includes the CN concrete providers for pricing and model mapping', () => {
    const source = readFileSync(resolve('src/views/admin/ChannelsView.vue'), 'utf8')
    expect(source).toContain('const platformOrder: GroupPlatform[] = CONCRETE_PLATFORM_OPTIONS.map')
    expect(source).toContain('const compositePlatforms: GroupPlatform[] = [...platformOrder]')

    const platforms = CONCRETE_PLATFORM_OPTIONS.map(option => option.value)
    expect(platforms).toEqual(expect.arrayContaining([
      'kimi',
      'zhipu',
      'deepseek',
      'minimax',
      'kiro',
      'qoder',
      'codebuddy',
      'codebuddy-china'
    ]))
  })
})
