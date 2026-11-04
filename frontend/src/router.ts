import { createRouter, createWebHistory, type RouteRecordRaw, type Router, type RouterHistory } from 'vue-router'

export const routes: RouteRecordRaw[] = [
  { path: '/', redirect: { name: 'campaign-new' } },
  {
    path: '/campaigns/new',
    name: 'campaign-new',
    component: () => import('./pages/CampaignCreatePage.vue'),
    meta: { title: 'New campaign' },
  },
  {
    // Stable, shareable URL: refresh and direct navigation load the campaign.
    path: '/campaigns/:id',
    name: 'campaign',
    component: () => import('./pages/CampaignComposerPage.vue'),
    props: true,
    meta: { title: 'Campaign' },
  },
  {
    path: '/:pathMatch(.*)*',
    name: 'not-found',
    component: () => import('./pages/NotFoundPage.vue'),
    meta: { title: 'Not found' },
  },
]

export function makeRouter(history: RouterHistory = createWebHistory()): Router {
  const router = createRouter({ history, routes, scrollBehavior: () => ({ top: 0 }) })
  router.afterEach((to) => {
    const t = typeof to.meta.title === 'string' ? to.meta.title : ''
    document.title = t ? `${t} · Campaign Composer` : 'Campaign Composer'
  })
  return router
}
