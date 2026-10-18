import { mount, tick, unmount } from "svelte";
import { afterEach, expect, it, vi } from "vitest";
import ApprovalCard from "../../src/components/ApprovalCard.svelte";
import ChannelEvents from "../../src/components/ChannelEvents.svelte";
import { MemoryStorage } from "../fixtures/memory-storage";
import DetailPanel from "../../src/components/DetailPanel.svelte";
import { createPUAAppChannels } from "../../src/app-channels";

const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => { while (cleanups.length) await cleanups.pop()?.(); vi.unstubAllGlobals(); document.body.replaceChildren(); FakeEventSource.instances = []; });
class FakeEventSource {
 static instances: FakeEventSource[] = [];
 onmessage: ((event: MessageEvent) => void) | null = null;
 onerror = null;
 readyState = 1;
 closed = false;
 constructor(readonly url: string) { FakeEventSource.instances.push(this); }
 addEventListener() {}
 close() { this.closed = true; }
 emit(id: number, type: string, data: object) {
  const channel = this.url.includes('/chat/runtime') ? 'dialogue' : 'execution';
  const sessionId = `session-${channel}`;
  this.onmessage?.(new MessageEvent('message', { data: JSON.stringify({schema:'agenthub.semantic-events.v1',cursor:id,mode:'replace',source:{eventId:id,type,sessionId,turnId:'turn-1'},events:[{id:`sem_${id}_0`,sourceEventId:id,index:0,type,sessionId,turnId:'turn-1',data}]}) }));
 }
}
function response(data: unknown) { return new Response(JSON.stringify(data), { headers: { 'Content-Type':'application/json' } }); }
function installFetch() {
 const requests: {path: string; init?: RequestInit}[] = [];
 vi.stubGlobal('EventSource', FakeEventSource);
 vi.stubGlobal('fetch', vi.fn(async (input, init) => {
  const path = String(input); requests.push({path,init});
  const channel = path.includes('/chat/runtime') ? 'dialogue' : 'execution';
  const generation = {generation:1,generationId:`gen-${channel}`,status:'running',createdAt:'2026-10-13T00:00:00Z'};
  const turn = {reference:`ref-${channel}`,turnId:'turn-1',closed:false,status:'active',startEventId:1,lastEventId:2,triggerPreview:`${channel} trigger`,generation};
  if (path.endsWith('/chat')) return response({binding:{kind:'profile',name:'default'}});
  if (path.endsWith('/status')) return response({resourceId:'project1.task1',generation,sessionState:'working',session:{id:`session-${channel}`,state:'running',currentTurnId:'turn-1'}});
  if (path.includes('/history/turns/ref-')) return response({turn,latestEventId:2,items:[{type:'message',role:'assistant',text:`${channel} history`,startEventId:1,endEventId:2}]});
  if (path.includes('/history/turns?')) return response({segments:[{generation,turns:[turn]}],page:{hasMore:false}});
  throw new Error(`unexpected request ${path}`);
 }));
 return requests;
}
function setup(dialogue: boolean) {
 const model = {...createPUAAppChannels().detail.current(),identity:'w:project1.task1',workspaceId:'w',resourceId:'project1.task1',currentUserName:'disksing'};
 const target=document.body.appendChild(document.createElement('div'));
 const component=mount(ChannelEvents,{target,props:{model,dialogue,onOpenFile:vi.fn(),onOpenLegacy:vi.fn()}});
 const dispose = async () => { await unmount(component); };
 cleanups.push(dispose);
 return {target,dispose};
}
it('renders both original timelines live, read-only, through isolated transports',async()=>{
 const requests=installFetch();
 const execution=setup(false), dialogue=setup(true);
 await vi.waitFor(()=>{
  expect(execution.target.textContent).toContain('execution history');
  expect(dialogue.target.textContent).toContain('dialogue history');
  expect(FakeEventSource.instances).toHaveLength(2);
 });
 const executionStream=FakeEventSource.instances.find(s=>!s.url.includes('/chat/runtime'))!;
 const dialogueStream=FakeEventSource.instances.find(s=>s.url.includes('/chat/runtime'))!;
 expect(new URL(dialogueStream.url,'http://localhost').searchParams.get('user')).toBe('disksing');
 expect(executionStream.url).not.toContain('user=');
 dialogueStream.emit(3,'message.assistant.delta',{text:'dialogue live reply',role:'assistant'});
 executionStream.emit(3,'tool.call',{schemaVersion:1,callId:'tool-1',operation:'start',toolKind:'command',name:'Command',summary:'echo execution',status:'running'});
 await vi.waitFor(()=>{
  expect(dialogue.target.querySelector('.agent-message-row')?.parentElement?.textContent).toContain('dialogue live reply');
  expect(execution.target.querySelector('.agent-activity-group')).not.toBeNull();
 });
 expect(execution.target.textContent).not.toContain('dialogue live reply');
 expect(dialogue.target.textContent).not.toContain('execution history');
 for(const target of [execution.target,dialogue.target]) {
  expect(target.querySelector('[data-component-owner="event-timeline"]')).not.toBeNull();
  expect(target.querySelector('textarea,input')).toBeNull();
  expect(target.querySelector('.history-turn')).toBeNull();
 }
 expect(requests.every(({init})=>!init?.method || init.method==='GET')).toBe(true);
 expect(requests.filter(({path})=>path.includes('/chat/runtime')).every(({init})=>new Headers(init?.headers).get('X-PUA-User')==='disksing')).toBe(true);
});
it('closes subscriptions on tab replacement and ignores frames from the old pane',async()=>{
 const requests=installFetch();
 const execution=setup(false);
 await vi.waitFor(()=>expect(FakeEventSource.instances).toHaveLength(1));
 const old=FakeEventSource.instances[0];
 await execution.dispose();cleanups.pop();
 expect(old.closed).toBe(true);
 const dialogue=setup(true);
 await vi.waitFor(()=>expect(dialogue.target.textContent).toContain('dialogue history'));
 const requestCount=requests.length;
 old.emit(9,'message.assistant.delta',{text:'stale execution output',role:'assistant'});
 await tick();
 expect(dialogue.target.textContent).not.toContain('stale execution output');
 expect(requests).toHaveLength(requestCount);
 await dialogue.dispose();cleanups.pop();
 expect(FakeEventSource.instances.every(s=>s.closed)).toBe(true);
});

it('cleans up the real detail tabs when switching resource, user or channel',async()=>{
 installFetch();
 vi.stubGlobal("localStorage", new MemoryStorage());
 const channel = createPUAAppChannels().detail;
 channel.publish({...channel.current(),identity:'w:project1.task1',workspaceId:'w',resourceId:'project1.task1',resourceType:'task',currentUserName:'disksing',loading:false,
  detail:{id:'project1.task1',type:'task',title:'Task',path:'project1/task1',files:[],artifacts:[],repos:[]}});
 const target=document.body.appendChild(document.createElement('div'));
 const component=mount(DetailPanel,{target,props:{channel}});
 cleanups.push(()=>unmount(component));
 await tick();
 const tab = (name: string) => [...target.querySelectorAll<HTMLButtonElement>('[role="tab"]')].find(b=>b.textContent?.trim()===name)!;
 tab('Execution events').click();
 await vi.waitFor(()=>expect(FakeEventSource.instances).toHaveLength(1));
 const first=FakeEventSource.instances[0];
 expect(tab('Dialog events')).toBeUndefined();
 channel.publish({...channel.current(),currentUserName:'other'});
 await vi.waitFor(()=>expect(FakeEventSource.instances).toHaveLength(2));
 expect(first.closed).toBe(true);
 const third=FakeEventSource.instances[1];
 channel.publish({...channel.current(),identity:'w:project1.task2',resourceId:'project1.task2',detail:{...channel.current().detail!,id:'project1.task2'}});
 await tick();
 // The selected tab may reset for a new resource; either way the old stream
 // must close before a stream for the next resource can be used.
 expect(third.closed).toBe(true);
 tab('Execution events').click();
 await vi.waitFor(()=>expect(FakeEventSource.instances.at(-1)?.url).toContain('/project1.task2/'));
 first.emit(99,'message.assistant.delta',{text:'late old-resource event',role:'assistant'});
 await tick();
 expect(target.textContent).not.toContain('late old-resource event');
});

 it('retains approval details and decisions without an action or reply input', async () => {
  const target = document.body.appendChild(document.createElement('div'));
  const component = mount(ApprovalCard, {target,props:{readOnly:true,generationId:'gen',contextIdentity:'dialogue',onApproval:vi.fn(),onToast:vi.fn(),item:{kind:'approval',approvalId:'a1',status:'pending',title:'Command approval',question:'Run this command?',detail:'echo inspection',options:[{optionId:'yes',name:'Allow once'}]}}});
  cleanups.push(()=>unmount(component));
  await tick();
  expect(target.textContent).toContain('Run this command?');
  expect(target.textContent).toContain('echo inspection');
  expect(target.textContent).toContain('Pending');
  expect(target.textContent).toContain('Allow once');
  expect(target.querySelector('input,button,form')).toBeNull();
 });

it.each([
 {dialogue:false,role:'user'}, {dialogue:false,role:'system'}, {dialogue:false,role:'agent'},
 {dialogue:true,role:'user'}, {dialogue:true,role:'system'}, {dialogue:true,role:'agent'},
])('shows full $role history and completed live Turns in dialogue=$dialogue without expanding a digest', async ({dialogue,role}) => {
 const channel = dialogue ? 'dialogue' : 'execution';
 const generation = {generation:1,generationId:`gen-${channel}`,status:'running',createdAt:'2026-10-13T00:00:00Z'};
 let closed = false;
 const historical = {reference:'ref-history',turnId:'turn-history',closed:true,status:'completed',triggerRole:role,triggerPreview:'Historical digest trigger',finalReplyPreview:'Historical digest reply',startEventId:1,lastEventId:3,endEventId:3,generation};
 const live = () => ({reference:'ref-live',turnId:'turn-1',closed,status:closed?'completed':'active',triggerRole:role,triggerPreview:'Live digest trigger',finalReplyPreview:'Live digest reply',startEventId:4,lastEventId:closed?7:5,endEventId:closed?7:undefined,generation});
 const terminalReads: string[] = [];
 vi.stubGlobal('EventSource',FakeEventSource);
 vi.stubGlobal('fetch',vi.fn(async (input) => {
  const path=String(input);
  if(path.endsWith('/chat')) return response({binding:{kind:'profile',name:'default'}});
  if(path.endsWith('/status')) return response({generation,sessionState:'working',session:{id:`session-${channel}`,state:'running',currentTurnId:'turn-1'}});
  if(path.includes('/history/turns?')) return response({segments:[{generation,turns:[historical,live()]}],page:{hasMore:false}});
  if(path.includes('/history/turns/ref-history')) return response({turn:historical,latestEventId:3,items:[
   {type:'message',role,text:`Historical full input ${role}`,startEventId:1,endEventId:1},
   {type:'message',role:'assistant',text:`Historical middle details ${role}`,startEventId:2,endEventId:2},
   {type:'tool',count:1,startEventId:3,endEventId:3},
  ]});
  if(path.includes('/history/turns/ref-live') || path.includes('/history/turns/by-id?')) {
   if(path.includes('/by-id?')) terminalReads.push(path);
   return response({turn:live(),latestEventId:closed?7:5,items:[{type:'message',role:'assistant',text:closed?`Canonical completed details ${role}`:`Active details ${role}`,startEventId:4,endEventId:closed?7:5}]});
  }
  if(path.includes('/events?')) return response({schema:'agenthub.semantic-events.v1',frames:[{
   schema:'agenthub.semantic-events.v1',cursor:5,mode:'replace',source:{eventId:5,type:'message.assistant.delta',sessionId:`session-${channel}`,turnId:'turn-1'},
   events:[{id:'sem_5_0',sourceEventId:5,index:0,type:'message.assistant.delta',sessionId:`session-${channel}`,turnId:'turn-1',data:{text:`Active details ${role}`}}],
  }],latestCursor:5,page:{hasMore:false,nextAfter:5}});
  throw new Error(`unexpected request ${path}`);
 }));
 const {target}=setup(dialogue);
 const messages=()=>[...target.querySelectorAll('.agent-message-bubble')].map(node=>node.textContent).join('\n');
 await vi.waitFor(()=>{
  expect(messages()).toContain(`Historical full input ${role}`);
  expect(messages()).toContain(`Historical middle details ${role}`);
  expect(messages()).toContain(`Active details ${role}`);
  expect(FakeEventSource.instances).toHaveLength(1);
 });
 expect(target.querySelector('.turn-collapsed-digest,.turn-collapsed-gap,.turn-collapse-again')).toBeNull();
 // Individual tool groups keep their existing click-to-expand interaction.
 expect(target.querySelector('details.agent-activity-group')).not.toBeNull();
 expect(target.querySelector('details.agent-activity-group')?.hasAttribute('open')).toBe(false);
 const stream=FakeEventSource.instances[0];
 stream.emit(6,'message.assistant.delta',{text:`Live update ${role}`});
 await vi.waitFor(()=>expect(messages()).toContain(`Live update ${role}`));
 closed=true;
 stream.emit(7,'turn.completed',{});
 await vi.waitFor(()=>{
  expect(terminalReads.length).toBeGreaterThan(0);
  expect(messages()).toContain(`Canonical completed details ${role}`);
 });
 // Replayed terminal frames / refreshed closed summaries cannot restore the
 // old role-based digest or hide already-loaded historical messages.
 stream.emit(7,'turn.completed',{});
 await tick();
 expect(messages()).toContain(`Historical middle details ${role}`);
 expect(messages()).toContain(`Canonical completed details ${role}`);
 expect(target.querySelector('.turn-collapsed-digest,.turn-collapsed-gap,.turn-collapse-again')).toBeNull();
});

it.each([false,true])('marks only the visible event channel read after history loads (dialogue=%s)',async dialogue=>{
 installFetch();
 const original=globalThis.fetch;
 let completed=3;
 const reads: Array<{path:string;body:unknown;user:string|null}>=[];
 vi.stubGlobal('fetch',vi.fn(async(input,init)=>{
  const path=String(input);
  if(path.endsWith('/read')) {
   reads.push({path,body:JSON.parse(String(init?.body)),user:new Headers(init?.headers).get('X-PUA-User')});
   return response({readTurnNumber:completed});
  }
  if(path.includes('/history/turns/by-id')) {
   const channel=dialogue?'dialogue':'execution';
   return response({turn:{reference:`ref-${channel}`,turnId:'turn-1',closed:true,status:'completed',startEventId:1,lastEventId:4,generation:{generation:1,generationId:`gen-${channel}`,status:'idle'}},items:[{type:'message',role:'assistant',text:'finished update',startEventId:2,endEventId:4}],latestEventId:4});
  }
  const result=await original(input,init);
  if(path.includes('/history/turns?')) {
   const page=await result.json();
   page.completedTurnNumber=completed;
   return response(page);
  }
  return result;
 }));
 const pane=setup(dialogue);
 const expected=`/api/workspaces/w/resources/project1.task1${dialogue?'/chat/runtime':''}/read`;
 await vi.waitFor(()=>expect(reads).toEqual([{path:expected,body:{throughTurnNumber:3},user:'disksing'}]));
 completed=4;
 FakeEventSource.instances[0].emit(4,'turn.completed',{});
 await vi.waitFor(()=>expect(reads).toHaveLength(2));
 expect(reads[1]).toEqual({path:expected,body:{throughTurnNumber:4},user:'disksing'});
 await pane.dispose();cleanups.pop();
 FakeEventSource.instances[0].emit(5,'turn.completed',{});
 await tick();expect(reads).toHaveLength(2);
});
