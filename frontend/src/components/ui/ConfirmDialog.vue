<script setup lang="ts">
// A modal confirmation for an irreversible action. The confirm button is
// disabled while the action is pending (no double submit); Escape and the
// secondary button close it only when nothing is pending.
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { useModalFocus } from '@/composables/useModalFocus'

const props = withDefaults(
  defineProps<{
    title: string
    confirmLabel: string
    pendingLabel: string
    cancelLabel?: string
    pending: boolean
    disabled?: boolean
    danger?: boolean
    testid: string
  }>(),
  { cancelLabel: 'Back', disabled: false, danger: false },
)
const emit = defineEmits<{ confirm: []; close: [] }>()

const dialog = ref<HTMLElement | null>(null)
const { trapTab } = useModalFocus(dialog)
function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape' && !props.pending) emit('close')
  else trapTab(e)
}
onMounted(() => document.addEventListener('keydown', onKey))
onBeforeUnmount(() => document.removeEventListener('keydown', onKey))
</script>

<template>
  <div class="backdrop">
    <div
      ref="dialog"
      class="dialog card"
      role="alertdialog"
      aria-modal="true"
      :aria-labelledby="`${testid}-title`"
      tabindex="-1"
      :data-testid="testid"
    >
      <header class="card-header">
        <h2 :id="`${testid}-title`" class="title">{{ title }}</h2>
      </header>
      <div class="card-body body">
        <slot />
        <div class="actions">
          <button
            type="button"
            class="btn"
            :class="danger ? 'btn-danger' : 'btn-primary'"
            :disabled="pending || disabled"
            :data-testid="`${testid}-confirm`"
            @click="emit('confirm')"
          >
            <span v-if="pending" class="spinner" aria-hidden="true" />
            {{ pending ? pendingLabel : confirmLabel }}
          </button>
          <button type="button" class="btn btn-secondary" :disabled="pending" :data-testid="`${testid}-back`" @click="emit('close')">
            {{ cancelLabel }}
          </button>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.backdrop {
  position: fixed;
  inset: 0;
  background: rgb(15 23 42 / 0.45);
  display: flex;
  align-items: flex-start;
  justify-content: center;
  padding: var(--space-8) var(--space-4);
  overflow-y: auto;
  z-index: 60;
}
.dialog {
  width: min(560px, 100%);
  outline: none;
}
.title {
  font-size: 19px;
  font-weight: 700;
}
.body {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  font-size: 14px;
}
.actions {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-3);
  margin-top: var(--space-2);
}
.btn-danger {
  background: var(--c-danger);
  color: #fff;
  border-color: var(--c-danger);
}
.btn-danger:not(:disabled):hover {
  background: #991b1b;
}
@media (max-width: 600px) {
  .backdrop {
    padding: 0;
  }
  .dialog {
    min-height: 100%;
    border-radius: 0;
  }
  .actions .btn {
    width: 100%;
  }
}
</style>
