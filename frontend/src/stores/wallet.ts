import { defineStore } from 'pinia'
import { computed, ref, shallowRef } from 'vue'
import {
  connect,
  disconnect,
  getConnection,
  getConnectors,
  reconnect,
  signMessage,
  watchConnection,
  watchConnectors,
  type Config,
  type Connector,
} from '@wagmi/core'
import type { Address } from 'viem'
import { api, configureAuth, errorMessage } from '@/lib/api'
import { getWagmiConfig, resolveOption, type WalletOption } from '@/lib/wagmi'
import type { Session } from '@/types/api'

const SESSION_KEY = 'earlypair.session.v1'

export type ModalStep = 'choose' | 'pending' | 'done'

function loadSession(): Session | null {
  try {
    const s = JSON.parse(localStorage.getItem(SESSION_KEY) || 'null') as Session | null
    if (s && Date.parse(s.expiresAt) > Date.now()) return s
  } catch {
    /* ignore */
  }
  return null
}

function saveSession(s: Session | null) {
  try {
    if (s) localStorage.setItem(SESSION_KEY, JSON.stringify(s))
    else localStorage.removeItem(SESSION_KEY)
  } catch {
    /* ignore */
  }
}

/** Wallet errors are verbose; users need the one line that matters. */
export function walletErrorMessage(err: unknown): string {
  const e = err as { shortMessage?: string; name?: string; message?: string; code?: number }
  if (e?.name === 'UserRejectedRequestError' || e?.code === 4001) return 'Request rejected in the wallet.'
  if (e?.name === 'ProviderNotFoundError') return 'That wallet is not available in this browser.'
  return e?.shortMessage || errorMessage(err)
}

export const useWallet = defineStore('wallet', () => {
  const cfg = shallowRef<Config | null>(null)
  const address = ref<Address | null>(null)
  const chainId = ref<number | null>(null)
  const connectorName = ref<string | null>(null)
  const connectors = shallowRef<readonly Connector[]>([])

  const modal = ref<ModalStep | null>(null)
  const pendingWallet = ref<string | null>(null)
  const error = ref<string | null>(null)

  const session = ref<Session | null>(loadSession())
  const signingIn = ref(false)
  const signInError = ref<string | null>(null)

  let generation = 0
  let initPromise: Promise<Config> | null = null

  const connected = computed(() => address.value !== null)
  const signedIn = computed(
    () =>
      !!session.value &&
      !!address.value &&
      session.value.address.toLowerCase() === address.value.toLowerCase() &&
      Date.parse(session.value.expiresAt) > Date.now(),
  )

  configureAuth(
    () => (signedIn.value ? session.value!.token : null),
    () => setSession(null),
  )

  function setSession(s: Session | null) {
    session.value = s
    saveSession(s)
  }

  function sync(conn: { address?: Address; chainId?: number; connector?: Connector }) {
    address.value = conn.address ?? null
    chainId.value = conn.chainId ?? null
    connectorName.value = conn.connector?.name ?? null
    // A session belongs to one address; switching accounts requires signing again.
    if (session.value && (!conn.address || session.value.address.toLowerCase() !== conn.address.toLowerCase())) {
      if (conn.address) setSession(null)
    }
  }

  function init(): Promise<Config> {
    if (initPromise) return initPromise
    initPromise = (async () => {
      const c = await getWagmiConfig()
      cfg.value = c
      connectors.value = getConnectors(c)
      watchConnectors(c, { onChange: (list) => (connectors.value = list) })
      watchConnection(c, { onChange: (conn) => sync(conn) })
      try {
        await reconnect(c)
      } catch {
        /* nothing to restore */
      }
      sync(getConnection(c))
      return c
    })()
    return initPromise
  }

  function openModal() {
    error.value = null
    modal.value = connected.value ? 'done' : 'choose'
    void init()
  }

  function closeModal() {
    if (modal.value === 'pending') cancel()
    modal.value = null
  }

  async function connectWith(option: WalletOption) {
    const c = await init()
    const { connector } = resolveOption(option, connectors.value)
    error.value = null
    if (!connector) {
      if (option.install) window.open(option.install, '_blank', 'noopener')
      else error.value = 'WalletConnect needs VITE_WALLETCONNECT_ID to be configured.'
      return
    }

    const mine = ++generation
    pendingWallet.value = option.name
    modal.value = 'pending'
    try {
      try {
        await connect(c, { connector })
      } catch (err) {
        if ((err as Error).name !== 'ConnectorAlreadyConnectedError') throw err
      }
      if (mine !== generation) {
        await disconnect(c, { connector }).catch(() => {})
        return
      }
      sync(getConnection(c))
      connectorName.value = option.name
      await signIn(mine)
      if (mine !== generation) return
      modal.value = 'done'
    } catch (err) {
      if (mine !== generation) return
      modal.value = 'choose'
      error.value = walletErrorMessage(err)
    }
  }

  /** Cancel an in-flight connection: the wallet may still prompt, but we ignore it. */
  function cancel() {
    generation++
    pendingWallet.value = null
    modal.value = 'choose'
  }

  async function signIn(mine = generation) {
    const c = await init()
    const who = address.value
    if (!who) return
    signingIn.value = true
    signInError.value = null
    try {
      const { nonce, message } = await api.nonce(who)
      const signature = await signMessage(c, { account: who, message })
      if (mine !== generation) return
      setSession(await api.verify({ address: who, nonce, signature }))
    } catch (err) {
      if (mine !== generation) return
      signInError.value = walletErrorMessage(err)
    } finally {
      signingIn.value = false
    }
  }

  async function disconnectWallet() {
    generation++
    const c = cfg.value
    if (c) await disconnect(c).catch(() => {})
    setSession(null)
    address.value = null
    chainId.value = null
    connectorName.value = null
    modal.value = null
  }

  return {
    cfg,
    address,
    chainId,
    connectorName,
    connectors,
    modal,
    pendingWallet,
    error,
    session,
    signingIn,
    signInError,
    connected,
    signedIn,
    init,
    openModal,
    closeModal,
    connectWith,
    cancel,
    signIn,
    disconnectWallet,
  }
})
