<script setup lang="ts">
// "Which targeting conditions are actually active": a read-only rendering of
// the stored, normalized rule (the same rule the backend compiles to SQL).
import { computed } from 'vue'
import type { SegmentRule } from '@/api/types'
import { presentRule, type ConditionKind } from '@/segment/presenter'

const props = defineProps<{ rule: SegmentRule; segmentVersion: number }>()

const view = computed(() => presentRule(props.rule))
const json = computed(() => JSON.stringify(props.rule, null, 2))

const KIND_LABEL: Record<ConditionKind, string> = {
  attribute: 'Profile',
  event: 'Event',
  behavior: 'Behavior',
  unknown: 'Unknown',
}
</script>

<template>
  <section class="card" aria-labelledby="targeting-heading" data-testid="segment-rule">
    <div class="card-header">
      <div>
        <p class="card-eyebrow"><span class="step-num">3</span> Active targeting</p>
        <h2 id="targeting-heading" class="card-title">Conditions applied to the audience</h2>
        <p class="card-subtitle">
          Validated by the backend · rule v{{ segmentVersion }} ·
          {{ view.conditionCount }} {{ view.conditionCount === 1 ? 'condition' : 'conditions' }}
        </p>
      </div>
    </div>
    <div class="card-body">
      <p class="match-label" data-testid="rule-match">
        <span class="match-pill" :class="view.match">{{ view.match === 'all' ? 'ALL' : 'ANY' }}</span>
        {{ view.matchLabel }}
      </p>

      <ul class="conditions" data-testid="rule-conditions">
        <li
          v-for="(c, i) in view.conditions"
          :key="`c${i}`"
          class="condition"
          :class="{ unknown: !c.known }"
          data-testid="rule-condition"
        >
          <span class="kind" :class="c.kind">{{ KIND_LABEL[c.kind] }}</span>
          <span class="cond-text">
            <span class="text">{{ c.text }}</span>
            <span v-if="c.hint" class="hint">{{ c.hint }}</span>
          </span>
        </li>
        <li v-for="(g, gi) in view.groups" :key="`g${gi}`" class="group" data-testid="rule-group">
          <p class="group-label">
            <span class="match-pill sm" :class="g.match">{{ g.match === 'all' ? 'ALL' : 'ANY' }}</span>
            Group {{ gi + 1 }} · {{ g.matchLabel }}
          </p>
          <ul class="conditions nested">
            <li
              v-for="(c, i) in g.conditions"
              :key="i"
              class="condition"
              :class="{ unknown: !c.known }"
              data-testid="rule-condition"
            >
              <span class="kind" :class="c.kind">{{ KIND_LABEL[c.kind] }}</span>
              <span class="cond-text">
                <span class="text">{{ c.text }}</span>
                <span v-if="c.hint" class="hint">{{ c.hint }}</span>
              </span>
            </li>
          </ul>
        </li>
      </ul>

      <p v-if="view.hasUnknown" class="notice notice-warning unknown-note" role="note">
        Some conditions could not be described by this version of the UI. Check the rule JSON below.
      </p>

      <p class="backend-note">
        Consent and contact availability are always enforced by the backend and are not part of the rule.
      </p>

      <details class="diagnostic json" data-testid="rule-json">
        <summary>Rule JSON (diagnostic)</summary>
        <pre>{{ json }}</pre>
      </details>
    </div>
  </section>
</template>

<style scoped>
.match-label {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-weight: 600;
  font-size: 14px;
  color: var(--c-text-2);
}
.match-pill {
  display: inline-block;
  padding: 1px 8px;
  border-radius: 4px;
  font-size: 11px;
  font-weight: 800;
  letter-spacing: 0.06em;
  background: var(--c-primary);
  color: #fff;
}
.match-pill.any {
  background: #0e7490;
}
.match-pill.sm {
  font-size: 10px;
  padding: 0 6px;
}
.conditions {
  list-style: none;
  margin: var(--space-3) 0 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
.conditions.nested {
  margin-top: var(--space-2);
}
.condition {
  display: flex;
  align-items: flex-start;
  gap: var(--space-3);
  padding: 10px 12px;
  border: 1px solid var(--c-border);
  border-radius: var(--radius);
  background: var(--c-surface);
}
.condition.unknown {
  border-color: var(--c-warning-border);
  background: var(--c-warning-soft);
}
.kind {
  flex: none;
  min-width: 68px;
  text-align: center;
  font-size: 11px;
  font-weight: 700;
  text-transform: uppercase;
  letter-spacing: 0.05em;
  padding: 2px 6px;
  border-radius: 4px;
  background: #f1f5f9;
  color: var(--c-text-2);
  margin-top: 1px;
}
.kind.event {
  background: #ecfeff;
  color: #155e75;
}
.kind.behavior {
  background: #f5f3ff;
  color: #5b21b6;
}
.kind.unknown {
  background: #fff;
  color: var(--c-warning);
}
.cond-text {
  display: flex;
  flex-direction: column;
  min-width: 0;
}
.text {
  font-weight: 600;
  overflow-wrap: anywhere;
}
.hint {
  font-size: 12.5px;
  color: var(--c-muted);
}
.group {
  padding: var(--space-3);
  border: 1px dashed var(--c-border-strong);
  border-radius: var(--radius);
  background: var(--c-surface-2);
}
.group-label {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: 13px;
  font-weight: 600;
  color: var(--c-text-2);
}
.unknown-note {
  margin-top: var(--space-3);
}
.backend-note {
  margin-top: var(--space-4);
  font-size: 12.5px;
  color: var(--c-muted);
}
.json {
  margin-top: var(--space-3);
}
</style>
