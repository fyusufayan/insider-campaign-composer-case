<script setup lang="ts">
import { RouterLink, RouterView } from 'vue-router'
import { provideLlmProvider } from '@/composables/useLlmProvider'

const provider = provideLlmProvider()
</script>

<template>
  <a class="skip-link" href="#main">Skip to content</a>
  <header class="topbar">
    <div class="topbar-inner">
      <RouterLink :to="{ name: 'campaign-new' }" class="brand" aria-label="Campaign Composer home">
        <span class="brand-mark" aria-hidden="true">
          <svg viewBox="0 0 32 32" width="28" height="28">
            <rect width="32" height="32" rx="8" fill="currentColor" />
            <path
              d="M9 21l5-10 4 7 2-3 3 6"
              stroke="#fff"
              stroke-width="2.4"
              fill="none"
              stroke-linecap="round"
              stroke-linejoin="round"
            />
          </svg>
        </span>
        <span class="brand-text">
          <span class="brand-name">Campaign Composer</span>
          <span class="brand-tag">AI-assisted audience &amp; messaging</span>
        </span>
      </RouterLink>
      <div class="topbar-right">
        <span
          v-if="provider === 'demo'"
          class="badge badge-warning"
          title="The backend runs the deterministic demo interpreter (keyword heuristic). No Claude model is called."
          data-testid="provider-badge"
        >
          <span class="dot" style="background: currentColor" aria-hidden="true" />
          Demo AI<span class="long">mode — not a live model</span>
        </span>
        <span v-else-if="provider === 'anthropic'" class="badge badge-primary" data-testid="provider-badge">
          <span class="dot" style="background: currentColor" aria-hidden="true" />
          Claude connected
        </span>
        <RouterLink :to="{ name: 'campaign-new' }" class="btn btn-primary btn-sm">New campaign</RouterLink>
      </div>
    </div>
  </header>
  <main id="main" class="main" tabindex="-1">
    <RouterView />
  </main>
  <footer class="footer">
    <span>Smart Campaign Composer · take-home demo · no real messages are sent</span>
  </footer>
</template>

<style scoped>
.skip-link {
  position: absolute;
  left: -9999px;
  top: 8px;
  z-index: 10;
  background: var(--c-surface);
  padding: 8px 12px;
  border-radius: var(--radius-sm);
}
.skip-link:focus {
  left: 8px;
}
.topbar {
  background: var(--c-surface);
  border-bottom: 1px solid var(--c-border);
  position: sticky;
  top: 0;
  z-index: 5;
}
.topbar-inner {
  max-width: 1240px;
  margin: 0 auto;
  padding: 10px var(--space-6);
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-4);
}
.brand {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  text-decoration: none;
  color: var(--c-text);
  border-radius: var(--radius-sm);
}
.brand-mark {
  color: var(--c-primary);
  display: inline-flex;
}
.brand-text {
  display: flex;
  flex-direction: column;
  line-height: 1.2;
}
.brand-name {
  font-weight: 700;
  font-size: 15px;
}
.brand-tag {
  font-size: 12px;
  color: var(--c-muted);
}
.topbar-right {
  display: flex;
  align-items: center;
  gap: var(--space-3);
}
.main {
  max-width: 1240px;
  margin: 0 auto;
  padding: var(--space-6);
  outline: none;
}
.footer {
  max-width: 1240px;
  margin: 0 auto;
  padding: var(--space-4) var(--space-6) var(--space-8);
  color: var(--c-muted);
  font-size: 12.5px;
}
@media (max-width: 640px) {
  .topbar-inner,
  .main {
    padding-left: var(--space-4);
    padding-right: var(--space-4);
  }
  .brand-tag {
    display: none;
  }
  .topbar-right .long {
    display: none;
  }
}
</style>
