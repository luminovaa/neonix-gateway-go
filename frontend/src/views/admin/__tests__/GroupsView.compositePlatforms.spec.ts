import { describe, expect, it } from 'vitest'
import { CONCRETE_PLATFORM_OPTIONS } from '@/constants/platforms'

describe('GroupsView Composite route options', () => {
  it('offers every native provider as a route target', () => {
    expect(CONCRETE_PLATFORM_OPTIONS.map((option) => option.value)).toEqual(
      expect.arrayContaining(['kimi', 'zhipu', 'deepseek', 'minimax', 'kiro', 'qoder', 'codebuddy', 'codebuddy-china'])
    )
  })
})
