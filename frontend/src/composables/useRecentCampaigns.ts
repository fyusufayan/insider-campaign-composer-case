// Campaigns opened on this device, kept in localStorage. CP6 has no listing
// endpoint; this is a convenience index only (id + name), never a source of
// campaign data — opening an entry always loads the campaign from the API.

import { ref } from 'vue'

export interface RecentCampaign {
  id: string
  name: string
  openedAt: string
}

const KEY = 'composer.recentCampaigns.v1'
const MAX = 8
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

function read(): RecentCampaign[] {
  try {
    const raw = localStorage.getItem(KEY)
    if (!raw) return []
    const v: unknown = JSON.parse(raw)
    if (!Array.isArray(v)) return []
    return v
      .filter(
        (x): x is RecentCampaign =>
          typeof x === 'object' &&
          x !== null &&
          typeof x.id === 'string' &&
          UUID.test(x.id) &&
          typeof x.name === 'string' &&
          typeof x.openedAt === 'string',
      )
      .slice(0, MAX)
  } catch {
    return []
  }
}

export function useRecentCampaigns() {
  const items = ref<RecentCampaign[]>(read())

  function remember(id: string, name: string) {
    const next = [{ id, name, openedAt: new Date().toISOString() }, ...read().filter((x) => x.id !== id)].slice(0, MAX)
    items.value = next
    try {
      localStorage.setItem(KEY, JSON.stringify(next))
    } catch {
      // storage unavailable (private mode, quota): the list is optional
    }
  }

  function forget(id: string) {
    const next = read().filter((x) => x.id !== id)
    items.value = next
    try {
      localStorage.setItem(KEY, JSON.stringify(next))
    } catch {
      // ignore
    }
  }

  return { items, remember, forget }
}

export function isCampaignId(s: string): boolean {
  return UUID.test(s.trim())
}
