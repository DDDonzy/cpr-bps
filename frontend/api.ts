export type Config = { enabled: boolean; accountIds: string[]; models: string[]; timeoutMs: number; onFailure: 'native_fallback' | 'reject' }
export type Account = { id: string; name: string; email: string | null; enabled: boolean; plan: string | null }
export type Settings = { revision: number; configuration: Config }
export type Catalog = { accounts: Account[]; models: string[]; warning: string | null }
declare global {
 interface Window {
  codexProxyPlugin: { theme: string; request(value: { method: string; path: string; contentType?: string; body?: ArrayBuffer }): Promise<{status: number; body: ArrayBuffer}> }
 }
}
export async function request<T>(method: string, path: string, data?: unknown): Promise<T> {
 const input: {method:string;path:string;contentType?:string;body?:ArrayBuffer}={method,path}
 if(data!==undefined) { input.contentType='application/json';input.body=new TextEncoder().encode(JSON.stringify(data)).buffer as ArrayBuffer }
 const reply=await window.codexProxyPlugin.request(input)
 const value=JSON.parse(new TextDecoder().decode(reply.body))
 if(reply.status>=400) throw new Error(value.error || value.message || '操作失败，请刷新重试')
 return value as T
}
export function normalized(value: Config): Config { return {enabled:value.enabled,accountIds:[...value.accountIds],models:[...value.models],timeoutMs:value.timeoutMs,onFailure:value.onFailure || 'native_fallback'} }
export function same(a: Config,b: Config): boolean { return a.onFailure===b.onFailure && a.enabled===b.enabled && a.timeoutMs===b.timeoutMs && JSON.stringify([...a.accountIds].sort())===JSON.stringify([...b.accountIds].sort()) && JSON.stringify([...a.models].sort())===JSON.stringify([...b.models].sort()) }
