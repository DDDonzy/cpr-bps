type Config = { enabled: boolean; accountIds: string[]; models: string[]; timeoutMs: number };
type Account = { id: string; name: string; email: string | null; enabled: boolean; plan: string | null };
type Settings = { revision: number; configuration: Config };
type Catalog = { accounts: Account[]; models: string[]; warning: string | null };
interface Bridge { theme: string; request(value: { method: string; path: string; contentType?: string; body?: ArrayBuffer }): Promise<{status: number; body: ArrayBuffer}>; }
interface Window { codexProxyPlugin: Bridge; }
const bridge = window.codexProxyPlugin;
document.documentElement.dataset.theme=bridge?.theme || 'light';
const el = <T extends HTMLElement>(id: string): T => document.getElementById(id) as T;
const state: { settings: Settings | null; catalog: Catalog; selectedMode: boolean; busy: boolean } = { settings:null, catalog:{accounts:[],models:[],warning:null}, selectedMode:false, busy:false };
function error(message = '') { el('error').textContent=message; el('error').hidden=!message; }
function busy(value: boolean) { state.busy=value; el<HTMLButtonElement>('save').disabled=value; el<HTMLButtonElement>('reload').disabled=value; el('save').textContent=value?'正在保存…':'保存设置'; }
function changed() { el('save-status').textContent='有未保存的修改'; el('save-status').className='hint'; }
async function request<T>(method: string, path: string, data?: unknown): Promise<T> {
 const input: {method:string;path:string;contentType?:string;body?:ArrayBuffer}={method,path};
 if(data!==undefined) { input.contentType='application/json';input.body=new TextEncoder().encode(JSON.stringify(data)).buffer as ArrayBuffer; }
 const reply=await bridge.request(input);const value=JSON.parse(new TextDecoder().decode(reply.body));
 if(reply.status>=400) throw new Error(value.error || value.message || '操作失败，请刷新重试');
 return value as T;
}
function normalized(value: Config): Config { return {enabled:value.enabled,accountIds:[...value.accountIds],models:[...value.models],timeoutMs:value.timeoutMs}; }
function same(a: Config,b: Config): boolean { return a.enabled===b.enabled && a.timeoutMs===b.timeoutMs && JSON.stringify([...a.accountIds].sort())===JSON.stringify([...b.accountIds].sort()) && JSON.stringify([...a.models].sort())===JSON.stringify([...b.models].sort()); }
function empty(container: HTMLElement, text: string) { const row=document.createElement('div');row.className='empty';row.textContent=text;container.append(row); }
function renderAccounts() {
 const config=state.settings!.configuration;el('account-picker').hidden=!state.selectedMode;
 el<HTMLInputElement>('all-accounts').checked=!state.selectedMode;el<HTMLInputElement>('selected-accounts').checked=state.selectedMode;
 el('account-count').textContent=state.selectedMode?`已选 ${config.accountIds.length} 个`:`${state.catalog.accounts.filter(a=>a.enabled).length} 个已启用`;
 el('account-hint').textContent=state.selectedMode?'仅从选定账号中调用 BPS，账号权限与配额仍由 CPR 管理。':'自动使用 CPR 授权范围内的可用 OpenAI OAuth 账号。';
 const container=el('accounts');container.replaceChildren();const term=el<HTMLInputElement>('account-search').value.toLowerCase();
 for(const account of state.catalog.accounts.filter(a=>`${a.name} ${a.email||''} ${a.id}`.toLowerCase().includes(term))) {
  const label=document.createElement('label');label.className='choice';const input=document.createElement('input');input.type='checkbox';input.checked=config.accountIds.includes(account.id);
  input.addEventListener('change',()=>{const ids=new Set(config.accountIds);input.checked?ids.add(account.id):ids.delete(account.id);config.accountIds=[...ids];changed();renderAccounts();});
  const info=document.createElement('div');const name=document.createElement('div');name.className='name';name.textContent=account.name||account.email||account.id;
  const detail=document.createElement('div');detail.className='detail';detail.textContent=[account.email!==account.name?account.email:null,account.plan,account.enabled?'已启用':'未启用'].filter(Boolean).join(' · ');
  info.append(name,detail);label.append(input,info);container.append(label);
 }
 if(!container.children.length) empty(container,'没有匹配的 OAuth 账号');
}
function renderModels() {
 const config=state.settings!.configuration;el('model-count').textContent=`已选 ${config.models.length} 个`;const container=el('models');container.replaceChildren();
 const term=el<HTMLInputElement>('model-search').value.toLowerCase();
 const models=[...new Set([...state.catalog.models,...config.models])].sort();
 for(const model of models.filter(m=>m.toLowerCase().includes(term))) {
  const label=document.createElement('label');label.className='choice';const input=document.createElement('input');input.type='checkbox';input.checked=config.models.includes(model);
  input.addEventListener('change',()=>{const set=new Set(config.models);input.checked?set.add(model):set.delete(model);config.models=[...set];changed();renderModels();});
  const name=document.createElement('span');name.className='name';name.textContent=model;label.append(input,name);container.append(label);
 }
 if(!container.children.length) empty(container,'没有匹配的模型');
}
function render() { const config=state.settings!.configuration;el<HTMLInputElement>('enabled').checked=config.enabled;el<HTMLInputElement>('timeout').value=String(config.timeoutMs/60000);renderAccounts();renderModels(); }
async function load() {
 error();busy(true);el('loading').hidden=false;
 try {
  const [settings,catalog]=await Promise.all([request<Settings>('GET','settings'),request<Catalog>('GET','catalog')]);
  settings.configuration=normalized(settings.configuration);state.settings=settings;state.catalog=catalog;state.selectedMode=settings.configuration.accountIds.length>0;
  render();el('form').hidden=false;el('save-status').textContent='已保存';if(catalog.warning) error(catalog.warning);
 } catch(e) { error(e instanceof Error?e.message:'无法读取设置'); } finally { busy(false);el('loading').hidden=true; }
}
async function save(event: Event) {
 event.preventDefault();if(state.busy||!state.settings)return;error();
 const config=normalized(state.settings.configuration);config.enabled=el<HTMLInputElement>('enabled').checked;config.timeoutMs=Number(el<HTMLInputElement>('timeout').value)*60000;
 if(!Number.isInteger(config.timeoutMs)||config.timeoutMs<60000||config.timeoutMs>14400000){error('等待时间须为 1 至 240 分钟');return;}
 if(config.enabled&&!config.models.length){error('启用 BPS 时至少选择一个模型');return;}
 if(state.selectedMode&&!config.accountIds.length){error('请选择至少一个账号，或切换为全部可用账号');return;}
 busy(true);
 try {
  try { await request('POST','settings',{revision:state.settings.revision,configuration:config}); } catch(original) {
   // 配置切换可能关闭旧页面调用：只查询确认，不重复提交。
   const actual=await request<Settings>('GET','settings');if(!same(actual.configuration,config))throw original;
  }
  try {
   const current=await request<Settings>('GET','settings');if(!same(current.configuration,config))throw new Error('配置已被更新，请重新加载核对');
   current.configuration=normalized(current.configuration);state.settings=current;render();el('save-status').textContent='已保存，立即生效';
  } catch {
   // 宿主收到旧 revision 的读取会重新挂载页面，避免把成功保存显示成失败。
   state.settings=null;el('save-status').textContent='已保存，正在刷新页面…';
  }
  el('save-status').className='hint success';
 } catch(e) { error(e instanceof Error?e.message:'保存失败'); } finally { busy(false);if(!state.settings)el<HTMLButtonElement>('save').disabled=true; }
}
el('save').addEventListener('click',save);el('reload').addEventListener('click',load);
el('enabled').addEventListener('change',()=>{if(state.settings)state.settings.configuration.enabled=el<HTMLInputElement>('enabled').checked;changed();});
el('timeout').addEventListener('input',changed);el('account-search').addEventListener('input',renderAccounts);el('model-search').addEventListener('input',renderModels);
for(const id of ['all-accounts','selected-accounts']) el(id).addEventListener('change',()=>{
 if(!state.settings)return;state.selectedMode=el<HTMLInputElement>('selected-accounts').checked;
 if(!state.selectedMode)state.settings.configuration.accountIds=[];changed();renderAccounts();
});
window.addEventListener('codex-proxy-themechange',()=>document.documentElement.dataset.theme=bridge.theme);
void load();
