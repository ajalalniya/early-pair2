/**
 * EVM wallet plumbing. Injected wallets are found through EIP-6963 discovery
 * (wagmi creates one connector per announced provider, keyed by its rdns), so
 * a `DETECTED` badge always reflects a wallet that actually announced itself.
 */
import { createConfig, http, type Config, type Connector } from '@wagmi/core'
import { arbitrum, base, bsc, mainnet } from '@wagmi/core/chains'
import { defineChain } from 'viem'
import { WALLETCONNECT_ID } from './env'

/** Robinhood Chain (Arbitrum Orbit). Chain id verified with eth_chainId on the public RPC. */
export const robinhood = defineChain({
  id: 4663,
  name: 'Robinhood Chain',
  nativeCurrency: { name: 'Ether', symbol: 'ETH', decimals: 18 },
  rpcUrls: { default: { http: ['https://rpc.mainnet.chain.robinhood.com/'] } },
  blockExplorers: { default: { name: 'Blockscout', url: 'https://robinhoodchain.blockscout.com' } },
  contracts: { multicall3: { address: '0xcA11bde05977b3631167028862bE2a173976CA11' } },
})

/**
 * Arc, Circle's L1. Chain id verified with eth_chainId. Gas is paid in USDC:
 * the native balance is 18-decimal, and the same money has a 6-decimal ERC-20
 * interface at 0x3600…0000 that pools pair against. There is no wrapped native.
 */
export const arc = defineChain({
  id: 5042,
  name: 'Arc',
  nativeCurrency: { name: 'USDC', symbol: 'USDC', decimals: 18 },
  rpcUrls: { default: { http: ['https://rpc.mainnet.arc.io'] } },
  blockExplorers: { default: { name: 'Arc Explorer', url: 'https://explorer.arc.io' } },
  contracts: { multicall3: { address: '0xcA11bde05977b3631167028862bE2a173976CA11' } },
})

export const EVM_CHAINS = [mainnet, base, bsc, arbitrum, robinhood, arc] as const

let config: Config | null = null

export async function getWagmiConfig(): Promise<Config> {
  if (config) return config
  const connectors = []
  // Heavy SDKs are loaded only when this module first runs, not with the feed.
  const { coinbaseWallet, walletConnect } = await import('@wagmi/connectors')
  connectors.push(coinbaseWallet({ appName: 'EarlyPair' }))
  if (WALLETCONNECT_ID) {
    connectors.push(
      walletConnect({
        projectId: WALLETCONNECT_ID,
        showQrModal: true,
        metadata: {
          name: 'EarlyPair',
          description: 'New-pair discovery and trading terminal',
          url: window.location.origin,
          icons: [`${window.location.origin}/favicon.svg`],
        },
      }),
    )
  }
  config = createConfig({
    chains: EVM_CHAINS,
    connectors,
    multiInjectedProviderDiscovery: true,
    transports: {
      [mainnet.id]: http(),
      [base.id]: http(),
      [bsc.id]: http(),
      [arbitrum.id]: http(),
      [robinhood.id]: http(),
      [arc.id]: http(),
    },
  })
  return config
}

export type WalletOptionId = 'metamask' | 'phantom' | 'rabby' | 'walletconnect' | 'coinbase'

export interface WalletOption {
  id: WalletOptionId
  name: string
  tag: string
  note: string
  tint: string
  rdns?: string[]
  install?: string
}

export const WALLET_OPTIONS: WalletOption[] = [
  { id: 'metamask', name: 'MetaMask', tag: 'MM', note: 'Browser extension · EVM', tint: 'oklch(0.82 0.14 60)', rdns: ['io.metamask', 'io.metamask.flask'], install: 'https://metamask.io/download/' },
  { id: 'phantom', name: 'Phantom', tag: 'PH', note: 'Solana · EVM', tint: 'oklch(0.72 0.14 295)', rdns: ['app.phantom'], install: 'https://phantom.com/download' },
  { id: 'rabby', name: 'Rabby', tag: 'RB', note: 'Pre-sign risk simulation', tint: 'oklch(0.78 0.12 235)', rdns: ['io.rabby'], install: 'https://rabby.io/' },
  { id: 'walletconnect', name: 'WalletConnect', tag: 'WC', note: 'Scan with a mobile wallet', tint: 'oklch(0.78 0.13 240)' },
  { id: 'coinbase', name: 'Coinbase Wallet', tag: 'CB', note: 'Smart wallet · passkey', tint: 'oklch(0.7 0.15 255)', rdns: ['com.coinbase.wallet'], install: 'https://www.coinbase.com/wallet/downloads' },
]

export type Badge = 'DETECTED' | 'INSTALL' | 'QR' | 'UNAVAILABLE'

/** Resolve the connector a wallet option should use, and how to badge it. */
export function resolveOption(option: WalletOption, connectors: readonly Connector[]): { connector: Connector | null; badge: Badge } {
  if (option.id === 'walletconnect') {
    const wc = connectors.find((c) => c.id === 'walletConnect') ?? null
    return { connector: wc, badge: wc ? 'QR' : 'UNAVAILABLE' }
  }
  const announced = connectors.find((c) => option.rdns?.includes(c.id)) ?? null
  if (announced) return { connector: announced, badge: 'DETECTED' }
  if (option.id === 'coinbase') {
    // The Coinbase SDK offers a passkey smart wallet without any extension.
    return { connector: connectors.find((c) => c.id === 'coinbaseWalletSDK') ?? null, badge: 'INSTALL' }
  }
  return { connector: null, badge: 'INSTALL' }
}
