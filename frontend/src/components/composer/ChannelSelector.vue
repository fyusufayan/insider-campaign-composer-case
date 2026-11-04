<script setup lang="ts">
// Delivery channel selection. Native checkboxes (keyboard: Tab + Space).
// Selecting neither channel is an explicit, valid choice.
import type { Channel } from '@/api/types'
import { CHANNELS } from '@/api/types'
import { CHANNEL_LABEL } from '@/composables/format'

const props = defineProps<{
  selected: Channel[]
  disabled: boolean
  saving: boolean
}>()

const emit = defineEmits<{ change: [channels: Channel[]] }>()

const DESCRIPTION: Record<Channel, string> = {
  sms: 'Users with a phone number and SMS consent',
  email: 'Users with an email address and email consent',
}

function toggle(ch: Channel, checked: boolean) {
  const set = new Set(props.selected)
  if (checked) set.add(ch)
  else set.delete(ch)
  emit(
    'change',
    CHANNELS.filter((c) => set.has(c)),
  )
}
</script>

<template>
  <fieldset class="channels" :disabled="disabled" data-testid="channel-selector" :aria-busy="saving ? 'true' : 'false'">
    <legend class="legend">
      Delivery channels
      <span v-if="saving" class="saving" role="status"><span class="spinner" aria-hidden="true" /> Saving…</span>
    </legend>
    <div class="options">
      <label v-for="ch in CHANNELS" :key="ch" class="option" :class="{ checked: selected.includes(ch) }">
        <input
          type="checkbox"
          class="checkbox"
          :name="`channel-${ch}`"
          :value="ch"
          :checked="selected.includes(ch)"
          :data-testid="`channel-${ch}`"
          @change="toggle(ch, ($event.target as HTMLInputElement).checked)"
        />
        <span class="option-text">
          <span class="option-label">{{ CHANNEL_LABEL[ch] }}</span>
          <span class="option-desc">{{ DESCRIPTION[ch] }}</span>
        </span>
      </label>
    </div>
    <p class="help">
      Pre-selected from the AI suggestion on the first interpretation. After that only your choices change the
      selection — re-interpreting never overrides it.
    </p>
  </fieldset>
</template>

<style scoped>
.channels {
  border: 0;
  margin: 0;
  padding: 0;
  min-width: 0;
}
.legend {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  font-weight: 700;
  font-size: 14px;
  padding: 0;
  margin-bottom: var(--space-2);
}
.saving {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-weight: 500;
  font-size: 13px;
  color: var(--c-primary-text);
}
.options {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--space-3);
}
.option {
  display: flex;
  align-items: flex-start;
  gap: var(--space-3);
  padding: 12px 14px;
  border: 1px solid var(--c-border-strong);
  border-radius: var(--radius);
  cursor: pointer;
  background: var(--c-surface);
  transition:
    border-color 0.12s,
    background-color 0.12s;
}
.option.checked {
  border-color: var(--c-primary);
  background: var(--c-primary-soft);
}
.option:focus-within {
  box-shadow: var(--focus-ring);
}
.channels:disabled .option {
  cursor: not-allowed;
  opacity: 0.7;
}
.checkbox {
  width: 18px;
  height: 18px;
  margin: 2px 0 0;
  accent-color: var(--c-primary);
  flex: none;
}
.checkbox:focus-visible {
  box-shadow: none;
}
.option-text {
  display: flex;
  flex-direction: column;
}
.option-label {
  font-weight: 700;
}
.option-desc {
  font-size: 12.5px;
  color: var(--c-muted);
}
.help {
  margin-top: var(--space-2);
  font-size: 12.5px;
  color: var(--c-muted);
}
@media (max-width: 520px) {
  .options {
    grid-template-columns: 1fr;
  }
}
</style>
