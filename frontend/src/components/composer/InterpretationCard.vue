<script setup lang="ts">
// "What the AI understood": the model's summary, assumptions, unsupported
// requests and content brief. These are untrusted proposals (validated by the
// backend) and are always rendered as escaped text.
import { computed } from 'vue'
import { parseContentBrief } from '@/api/decode'
import type { InterpretationRecord } from '@/api/types'
import AiSourceBadge from './AiSourceBadge.vue'

const props = defineProps<{ interpretation: InterpretationRecord; contentBrief: unknown }>()

const brief = computed(() => parseContentBrief(props.contentBrief))

const offerLabel = computed(() => {
  const o = brief.value?.offer
  if (!o) return ''
  switch (o.kind) {
    case 'percent_discount':
      return `${o.value}% discount`
    case 'free_shipping':
      return 'Free shipping'
    default:
      return 'No offer'
  }
})

function humanize(s: string): string {
  return s.replace(/_/g, ' ')
}
</script>

<template>
  <section class="card" aria-labelledby="understood-heading" data-testid="interpretation-card">
    <div class="card-header">
      <div>
        <p class="card-eyebrow"><span class="step-num">2</span> What the AI understood</p>
        <h2 id="understood-heading" class="card-title">Interpretation</h2>
      </div>
      <AiSourceBadge :model="interpretation.model" :prompt-version="interpretation.prompt_version" />
    </div>
    <div class="card-body">
      <p class="summary" data-testid="interpretation-summary">{{ interpretation.summary }}</p>
      <p class="field-help disclaimer">
        AI-written summary. The targeting actually applied is listed under <em>Active targeting</em>.
      </p>

      <div class="notes">
        <div class="note-block">
          <h3 class="note-title">Assumptions</h3>
          <ul v-if="interpretation.assumptions.length" class="note-list" data-testid="assumptions">
            <li v-for="(a, i) in interpretation.assumptions" :key="i">{{ a }}</li>
          </ul>
          <p v-else class="empty">None stated.</p>
        </div>
        <div class="note-block">
          <h3 class="note-title">Not supported — ignored for targeting</h3>
          <ul v-if="interpretation.unsupported.length" class="note-list warn" data-testid="unsupported">
            <li v-for="(u, i) in interpretation.unsupported" :key="i">{{ u }}</li>
          </ul>
          <p v-else class="empty">Nothing — every request in the brief was expressible.</p>
        </div>
      </div>

      <div class="content-brief" data-testid="content-brief">
        <h3 class="note-title">Content brief <span class="for-later">(used for message generation later)</span></h3>
        <dl v-if="brief" class="cb-grid">
          <div>
            <dt>Goal</dt>
            <dd>{{ humanize(brief.goal) }}</dd>
          </div>
          <div>
            <dt>Tone</dt>
            <dd>{{ brief.tone }}</dd>
          </div>
          <div>
            <dt>Length</dt>
            <dd>{{ brief.length }}</dd>
          </div>
          <div>
            <dt>Offer</dt>
            <dd data-testid="offer">{{ offerLabel }}</dd>
          </div>
        </dl>
        <p v-else class="empty">No content brief available.</p>
      </div>
    </div>
  </section>
</template>

<style scoped>
.summary {
  font-size: 15.5px;
  line-height: 1.55;
  overflow-wrap: anywhere;
}
.disclaimer {
  margin-top: 6px;
  font-size: 12.5px;
}
.notes {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--space-4);
  margin-top: var(--space-5);
}
.note-title {
  font-size: 13px;
  font-weight: 700;
  color: var(--c-text-2);
  margin-bottom: 6px;
}
.note-list {
  margin: 0;
  padding-left: 18px;
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 14px;
  overflow-wrap: anywhere;
}
.note-list.warn li::marker {
  color: var(--c-no-consent);
}
.empty {
  font-size: 13.5px;
  color: var(--c-muted);
}
.content-brief {
  margin-top: var(--space-5);
  padding-top: var(--space-4);
  border-top: 1px solid var(--c-border);
}
.for-later {
  font-weight: 500;
  color: var(--c-muted);
}
.cb-grid {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: var(--space-3);
  margin: 0;
}
.cb-grid dt {
  font-size: 12px;
  color: var(--c-muted);
  text-transform: uppercase;
  letter-spacing: 0.04em;
  font-weight: 600;
}
.cb-grid dd {
  margin: 2px 0 0;
  font-weight: 600;
  overflow-wrap: anywhere;
  text-transform: capitalize;
}
@media (max-width: 640px) {
  .notes {
    grid-template-columns: 1fr;
  }
  .cb-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}
</style>
