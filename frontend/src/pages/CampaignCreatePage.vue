<script setup lang="ts">
import { onBeforeUnmount, ref } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import { ApiError, api, isApiError } from '@/api/client'
import BriefInput from '@/components/composer/BriefInput.vue'
import { formatDateTime } from '@/composables/format'
import { isCampaignId, useRecentCampaigns } from '@/composables/useRecentCampaigns'
import { useCampaignStore } from '@/stores/campaign'

const router = useRouter()
const store = useCampaignStore()
const recent = useRecentCampaigns()

// "Revise brief in a new campaign" passes the previous text via history
// state (never via the URL). Only string values are accepted.
function readPrefill(): { name: string; brief: string } {
  const st: unknown = router.options.history.state
  if (st && typeof st === 'object' && 'prefill' in st) {
    const p = (st as { prefill: unknown }).prefill
    if (p && typeof p === 'object') {
      const { name, brief } = p as Record<string, unknown>
      return { name: typeof name === 'string' ? name : '', brief: typeof brief === 'string' ? brief : '' }
    }
  }
  return { name: '', brief: '' }
}
const prefill = readPrefill()

const pending = ref(false)
const error = ref<ApiError | null>(null)
let controller: AbortController | null = null

async function create(value: { name: string; brief: string }) {
  if (pending.value) return // no duplicate submissions
  pending.value = true
  error.value = null
  controller = new AbortController()
  try {
    const c = await api.createCampaign(value, controller.signal)
    store.adopt(c)
    recent.remember(c.id, c.name)
    await router.push({ name: 'campaign', params: { id: c.id } })
  } catch (e) {
    if (isApiError(e) && e.isAbort) return
    error.value = isApiError(e)
      ? e
      : new ApiError({ kind: 'invalid_response', code: 'client_error', message: 'An unexpected error occurred.' })
  } finally {
    pending.value = false
    controller = null
  }
}

onBeforeUnmount(() => controller?.abort())

const openId = ref('')
const openError = ref<string | null>(null)
function openById() {
  const id = openId.value.trim()
  if (!isCampaignId(id)) {
    openError.value = 'Enter a campaign ID (UUID), e.g. 0b9c…-…'
    return
  }
  openError.value = null
  void router.push({ name: 'campaign', params: { id: id.toLowerCase() } })
}
</script>

<template>
  <div class="page">
    <header class="page-header">
      <p class="card-eyebrow">New campaign</p>
      <h1>Describe your campaign</h1>
      <p class="lede">
        Write a brief in plain language. The AI proposes a typed audience rule, the backend validates it, and you see
        exactly who matches — before any content or sending.
      </p>
    </header>

    <div class="layout">
      <section class="card" aria-labelledby="brief-heading">
        <div class="card-header">
          <div>
            <p class="card-eyebrow"><span class="step-num">1</span> Brief</p>
            <h2 id="brief-heading" class="card-title">What do you want to send, and to whom?</h2>
          </div>
        </div>
        <div class="card-body">
          <BriefInput
            :pending="pending"
            :server-error="error"
            :initial-name="prefill.name"
            :initial-brief="prefill.brief"
            @submit="create"
          />
        </div>
      </section>

      <aside class="side">
        <section class="card side-card" aria-labelledby="how-heading">
          <div class="card-body">
            <h2 id="how-heading" class="side-title">How it works</h2>
            <ol class="steps">
              <li>
                <strong>Brief.</strong> You describe the campaign; it is saved as written.
              </li>
              <li>
                <strong>Interpretation.</strong> The AI proposes targeting conditions, assumptions and a content brief.
                Everything it proposes is validated by the backend.
              </li>
              <li>
                <strong>Audience.</strong> Live counts from the customer database, split by contact and consent per
                channel.
              </li>
              <li><strong>Channels.</strong> Choose SMS, email, both — or neither for now.</li>
            </ol>
          </div>
        </section>

        <section class="card side-card" aria-labelledby="open-heading">
          <div class="card-body">
            <h2 id="open-heading" class="side-title">Open a campaign</h2>
            <form class="open-form" novalidate @submit.prevent="openById">
              <label class="visually-hidden" for="open-id">Campaign ID</label>
              <input
                id="open-id"
                v-model="openId"
                class="input mono"
                placeholder="Campaign ID"
                autocomplete="off"
                spellcheck="false"
                :aria-invalid="openError ? 'true' : 'false'"
                :aria-describedby="openError ? 'open-id-error' : undefined"
              />
              <button type="submit" class="btn btn-secondary">Open</button>
            </form>
            <p v-if="openError" id="open-id-error" class="field-error">{{ openError }}</p>

            <template v-if="recent.items.value.length">
              <h3 class="recent-title">Recently opened on this device</h3>
              <ul class="recent" data-testid="recent-campaigns">
                <li v-for="r in recent.items.value" :key="r.id">
                  <RouterLink :to="{ name: 'campaign', params: { id: r.id } }" class="recent-link">
                    <span class="recent-name">{{ r.name }}</span>
                    <span class="recent-meta">{{ formatDateTime(r.openedAt) }}</span>
                  </RouterLink>
                </li>
              </ul>
            </template>
          </div>
        </section>
      </aside>
    </div>
  </div>
</template>

<style scoped>
.page-header {
  margin-bottom: var(--space-6);
  max-width: 760px;
}
.page-header h1 {
  font-size: 28px;
  font-weight: 750;
}
.lede {
  margin-top: var(--space-2);
  color: var(--c-text-2);
  font-size: 16px;
}
.layout {
  display: grid;
  grid-template-columns: minmax(0, 1fr) 340px;
  gap: var(--space-6);
  align-items: start;
}
.side {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}
.side-card .card-body {
  padding-top: var(--space-5);
}
.side-title {
  font-size: 15px;
  font-weight: 700;
  margin-bottom: var(--space-3);
}
.steps {
  margin: 0;
  padding-left: 20px;
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  color: var(--c-text-2);
  font-size: 14px;
}
.open-form {
  display: flex;
  gap: var(--space-2);
}
.recent-title {
  margin-top: var(--space-5);
  margin-bottom: var(--space-2);
  font-size: 13px;
  color: var(--c-muted);
  font-weight: 600;
}
.recent {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.recent-link {
  display: flex;
  flex-direction: column;
  padding: 8px 10px;
  border-radius: var(--radius-sm);
  text-decoration: none;
  color: var(--c-text);
}
.recent-link:hover {
  background: var(--c-surface-2);
}
.recent-name {
  font-weight: 600;
  font-size: 14px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.recent-meta {
  font-size: 12px;
  color: var(--c-muted);
}
@media (max-width: 960px) {
  .layout {
    grid-template-columns: 1fr;
  }
}
</style>
