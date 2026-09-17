import { createRouter, createWebHistory } from 'vue-router'
import PoolsView from '@/views/PoolsView.vue'

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', redirect: { path: '/pools' } },
    { path: '/pools', name: 'pools', component: PoolsView, meta: { title: 'New Pools' } },
    { path: '/portfolio', name: 'portfolio', component: () => import('@/views/PortfolioView.vue'), meta: { title: 'Portfolio' } },
    { path: '/watchlist', name: 'watchlist', component: () => import('@/views/WatchlistView.vue'), meta: { title: 'Watchlist' } },
    { path: '/:pathMatch(.*)*', redirect: { path: '/pools' } },
  ],
  scrollBehavior(to, from) {
    // Filter changes rewrite the query string; they must not jump the page.
    if (to.path === from.path) return false
    return { top: 0 }
  },
})

router.afterEach((to) => {
  const title = to.meta.title as string | undefined
  document.title = title ? `${title} · EarlyPair` : 'EarlyPair'
})
