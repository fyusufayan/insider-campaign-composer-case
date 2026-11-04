// The LLM provider selected at API startup (GET /readyz → llm_provider).
// Fetched once by the app shell and provided to descendants. Unknown when
// the probe fails; components then rely on interpretation.model only.

import { inject, onBeforeUnmount, provide, ref, type InjectionKey, type Ref } from 'vue'
import { api } from '@/api/client'

export type LlmProvider = 'demo' | 'anthropic' | 'unknown'

const KEY: InjectionKey<Ref<LlmProvider>> = Symbol('llmProvider')

export function provideLlmProvider(): Ref<LlmProvider> {
  const provider = ref<LlmProvider>('unknown')
  const controller = new AbortController()
  api
    .readiness(controller.signal)
    .then((r) => {
      provider.value = r.llm_provider === 'demo' || r.llm_provider === 'anthropic' ? r.llm_provider : 'unknown'
    })
    .catch(() => {
      provider.value = 'unknown'
    })
  onBeforeUnmount(() => controller.abort())
  provide(KEY, provider)
  return provider
}

export function useLlmProvider(): Ref<LlmProvider> {
  return inject(KEY, ref<LlmProvider>('unknown'))
}
