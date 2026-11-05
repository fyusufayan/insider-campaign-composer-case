<script setup lang="ts">
// The rendered email for the fixed fictional example recipient, shown in a
// fully sandboxed iframe (sandbox="": no scripts, no same-origin, no
// forms, no navigation). The document is built from escaped text only, so
// model- or user-written text can never become markup.
import { computed } from 'vue'
import type { RenderedEmail } from '@/api/types'
import { escapeHtml } from '@/composables/escape'

const props = defineProps<{ email: RenderedEmail | null; unavailable: string }>()

const srcdoc = computed(() => {
  const e = props.email
  if (!e) return ''
  return (
    '<!doctype html><html><head><meta charset="utf-8">' +
    '<style>body{margin:0;padding:16px;font:14.5px/1.55 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;color:#0f172a;background:#fff}' +
    '.subject{font-weight:650;font-size:16px;margin:0 0 12px}.body{white-space:pre-wrap;overflow-wrap:anywhere;margin:0}' +
    '.footer{margin-top:20px;padding-top:10px;border-top:1px solid #e2e8f0;color:#556274;font-size:12.5px;white-space:pre-wrap}</style>' +
    '</head><body>' +
    `<p class="subject">${escapeHtml(e.subject)}</p>` +
    `<p class="body">${escapeHtml(e.body)}</p>` +
    `<p class="footer">${escapeHtml(e.footer)}</p>` +
    '</body></html>'
  )
})
</script>

<template>
  <figure class="email-preview" data-testid="email-preview">
    <figcaption>Email preview</figcaption>
    <template v-if="email">
      <p class="subject-line" data-testid="email-preview-subject"><span>Subject:</span> {{ email.subject }}</p>
      <iframe
        class="frame"
        sandbox=""
        referrerpolicy="no-referrer"
        title="Email preview with example values"
        :srcdoc="srcdoc"
        data-testid="email-preview-frame"
      />
    </template>
    <p v-else class="unavailable" data-testid="email-preview-unavailable">{{ unavailable }}</p>
  </figure>
</template>

<style scoped>
.email-preview {
  margin: 0;
}
figcaption {
  font-size: 12px;
  font-weight: 600;
  color: var(--c-muted);
  text-transform: uppercase;
  letter-spacing: 0.04em;
  margin-bottom: 6px;
}
.subject-line {
  font-size: 13px;
  margin-bottom: 6px;
  overflow-wrap: anywhere;
}
.subject-line span {
  color: var(--c-muted);
}
.frame {
  width: 100%;
  height: 300px;
  border: 1px solid var(--c-border);
  border-radius: var(--radius-sm);
  background: #fff;
}
.unavailable {
  font-size: 13px;
  color: var(--c-muted);
  font-style: italic;
}
</style>
