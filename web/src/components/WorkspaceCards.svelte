<script lang="ts">
  import {onMount, untrack, type Snippet} from "svelte";
  import type {WorkspaceTree} from "../models/workspace";
  import CardContent from "./CardContent.svelte";
  import MagpieQuota from "./MagpieQuota.svelte";
  import {cardObservation, collapsedReadout, labels, project, sources, styles, summarize, type Card, type Content, type Kind, type Sample, type Summary} from "./workspace-cards";

  let {workspace,user,tree,treeError,onSelect,onClose,panelAction}: {
    workspace:string; user:string; tree:WorkspaceTree|null; treeError:string;
    onSelect:(id:string)=>Promise<void>; onClose:()=>void;
    panelAction:Snippet;
  } = $props();
  let cards=$state<Card[]>([]);
  let samples=$state<Sample[]>([]);
  let adding=$state(false), loaded=$state(false), busy=$state(false), error=$state("");
  let kind=$state<Kind>("stat"), source=$state("running"), title=$state("");
  let dragging=$state("");
  let offline=$state(false);
  let quotaData=$state<Content | null>(null);
  const hasQuota=$derived(cards.some(card=>card.source==="magpie-quota"));
  $effect(()=>{
    if (!hasQuota) {
      quotaData=null;
      return;
    }
    const controller=new AbortController();
    const url=`/api/workspaces/${encodeURIComponent(workspace)}/cards/magpie-quota`;
    const headers={"X-PUA-User":user};
    let timer:ReturnType<typeof setTimeout>;
    async function refresh() {
      try {
        const response=await fetch(url,{headers,signal:controller.signal});
        const next=response.ok ? {quota:await response.json()} : {};
        if (!controller.signal.aborted) quotaData=next;
      } catch {
        if (!controller.signal.aborted) quotaData={};
      } finally {
        if (!controller.signal.aborted) timer=setTimeout(refresh,15000);
      }
    }
    void refresh();
    return ()=>{controller.abort();clearTimeout(timer);};
  });
  const summary=$derived(tree && !treeError ? summarize(tree) : null);
  const cacheKey=$derived(`pua-card-samples:${JSON.stringify([workspace,user])}`);
  let serial=0, disposed=false;
  const abort=new AbortController();
  async function request(body?:object) {
    const response=await fetch(`/api/workspaces/${encodeURIComponent(workspace)}/cards`,{
      method:body ? "POST":"GET", signal:abort.signal,
      headers:{"X-PUA-User":user,"Content-Type":"application/json"},body:body ? JSON.stringify(body):undefined,
    });
    if (!response.ok) throw new Error((await response.json()).error || `Cards: ${response.status}`);
    return response.json();
  }
  async function load() {
    if (busy || disposed) return;
    const current=++serial;
    try {
      const next=await request();
      if (current!==serial || disposed) return;
      cards=next; loaded=true; error="";
    } catch(reason) {if(current===serial && !disposed) error=String(reason);}
  }
  async function change(body:object) {
    if (busy) return;
    busy=true; serial++;
    try {
      const result=await request(body);
      if(disposed) return;
      cards=result.cards; error=""; return true;
    } catch(reason) {if(!disposed) error=String(reason);}
    finally {busy=false;}
  }
  function layout(card:Card, patch:object) {
    return change({action:"layout",id:card.id,version:card.version,...patch});
  }
  async function add() {
    if(await change({action:"create",kind,source,title:title.trim() || labels[source]})) {adding=false;title="";}
  }
  onMount(() => {
    try {
      const stored=JSON.parse(sessionStorage.getItem(cacheKey) ?? "[]");
      if(Array.isArray(stored)) samples=stored.filter(item=>item && Number.isFinite(Date.parse(item.time)) && ["tasks","running","running-tasks"].every(source=>Number.isFinite(item.counts?.[source]))).slice(-120);
    } catch {samples=[];}
    const unsubscribe=cardObservation.subscribe(observation=>{
      if(observation?.workspace!==workspace || observation.user!==user) return;
      offline=!observation.tree;
      if(observation.tree) sample(summarize(observation.tree),cacheKey);
    });
    void load();
    const timer=setInterval(load,4000);
    return ()=>{disposed=true;abort.abort();clearInterval(timer);unsubscribe();};
  });
  $effect(()=>{
    const current=summary, key=cacheKey;
    if(!current) return;
    untrack(()=>sample(current,key));
  });
  function sample(current:Summary,key:string) {
      const counts=Object.fromEntries(Object.entries(current.bySource).map(([key,items])=>[key,items.length]));
      const last=samples.at(-1), now=Date.now();
      if(last && now-Date.parse(last.time)<4000 && Object.keys(counts).every(key=>counts[key]===last.counts[key])) return;
      samples=[...samples.slice(-119),{time:new Date(now).toISOString(),counts}];
      try {sessionStorage.setItem(key,JSON.stringify(samples));} catch {}
  }
  async function select(id:string) {
    try {await onSelect(id);onClose();} catch(reason){error=String(reason);}
  }
</script>

<div class="board">
  <header>
    <h2>Workspace cards</h2>
    <button class="add" aria-label="Add card" title="Add card" disabled={!loaded} onclick={()=>adding=!adding} aria-expanded={adding}>+</button>
    {@render panelAction()}
  </header>
  {#if adding}
    <form onsubmit={(event)=>{event.preventDefault();void add();}}>
      <label>Style<select value={kind} onchange={(event)=>{kind=event.currentTarget.value as Kind;source=sources[kind][0];}}>
        {#each Object.entries(styles).filter(([key])=>key!=="html") as [key,name]}<option value={key}>{name}</option>{/each}
      </select></label>
      <label>Data source<select bind:value={source}>{#each sources[kind] as key}<option value={key}>{labels[key]}</option>{/each}</select></label>
      <label>Title<input bind:value={title} maxlength="150" placeholder={labels[source]} /></label>
      <div><button class="primary" disabled={busy}>Add to workspace</button> <button type="button" onclick={()=>adding=false}>Cancel</button></div>
    </form>
  {/if}
  {#if error}<p role="alert">{error} <button onclick={load}>Retry</button></p>{/if}
  <div class="stack">
    {#each cards as card,index (card.id)}
      {@const data=card.source==="magpie-quota" ? quotaData : project(card,offline ? null:summary,samples)}
      <article class:stat-card={card.kind === "stat"} draggable="true" ondragstart={()=>dragging=card.id} ondragend={()=>dragging=""}
        ondragover={(event)=>event.preventDefault()} ondrop={(event)=>{event.preventDefault();const item=cards.find(item=>item.id===dragging);if(item && item.id!==card.id) void layout(item,{before:card.id});dragging="";}}>
        <div class="heading">
          <button class="name" aria-expanded={!card.collapsed} onclick={()=>layout(card,{collapsed:!card.collapsed})}>
            <strong>{card.title}</strong>
            {#if card.collapsed}<span class="brief">{collapsedReadout(card.kind, data, card.hiddenQuotaKeys)}</span>{/if}
            <span class="chevron" aria-hidden="true"></span>
          </button>
          <details><summary aria-label={`Options for ${card.title}`}>···</summary><div class="menu">
            <button disabled={index===0||busy} onclick={()=>layout(card,{before:cards[index-1].id})}>Move up</button>
            <button disabled={index===cards.length-1||busy} onclick={()=>layout(card,{before:cards[index+2]?.id ?? ""})}>Move down</button>
            <button onclick={()=>layout(card,{height:card.height===180 ? 320:card.height===320 ? 0:180})}>Resize</button>
            <button onclick={()=>change({action:"remove",id:card.id,version:card.version})}>Remove</button>
          </div></details>
        </div>
        {#if !card.collapsed}
          <div class="body" style:height={card.height ? `${card.height}px`:undefined}>
            {#if card.kind === "quota"}
              {#if data?.quota}
                <MagpieQuota quota={data.quota} hiddenKeys={card.hiddenQuotaKeys ?? []} {busy}
                  onFilter={(hiddenQuotaKeys)=>layout(card,{hiddenQuotaKeys})}/>
              {:else}<small>{data ? "AgentHub quota unavailable" : "Loading quota…"}</small>{/if}
            {:else if data}<CardContent kind={card.kind} source={card.source} {data} onSelect={select}/>{:else}<small>Data unavailable</small>{/if}
          </div>
          <footer>{card.source === "agent" ? "Agent" : `System · ${labels[card.source]}`}{#if card.updated} · {new Date(card.updated).toLocaleTimeString()}{/if}</footer>
        {/if}
      </article>
    {:else}{#if loaded}<p>Add a card to follow this workspace.</p>{/if}{/each}
  </div>
</div>
<style>
  .board {
    --card-surface:#fffef8;
    --card-line:#d6d9cc;
    --card-accent:#35684a;
    --card-muted:#60695b;
    container-type:inline-size;
    min-width:0;
    height:100%;
    overflow:auto;
    color:#24392c;
    color-scheme:light;
    background:#f7f7ef;
  }
  header,.heading,.name { display:flex; align-items:center; gap:8px }
  header { padding:15px 19px; border-bottom:1px solid var(--card-line) }
  h2 { display:flex; align-items:center; gap:8px; flex:1; margin:0; font-size:1rem; letter-spacing:-.02em }
  h2::before { content:""; width:13px; height:13px; border:1px solid var(--card-accent); background:repeating-linear-gradient(0deg,transparent 0 3px,#35684a88 3px 4px),repeating-linear-gradient(90deg,transparent 0 3px,#35684a88 3px 4px) }
  button,summary { cursor:pointer }
  button:disabled { opacity:.5; cursor:default }
  button,select,input { font:inherit; color:inherit; border:1px solid var(--card-line); border-radius:6px; background:var(--card-surface) }
  button { padding:5px 9px }
  .add { width:30px; height:30px; padding:0; font-size:20px; line-height:1; background:transparent }
  header :global(.cards-dismiss) { border:1px solid var(--card-line); background:transparent; color:inherit }
  header :global(.cards-dismiss:hover) { background:var(--card-surface) }
  .primary { color:#fff; background:var(--card-accent); border-color:transparent }
  .stack { display:grid; gap:9px; padding:15px }
  form,article,.menu { border:1px solid var(--card-line); border-radius:12px; background:var(--card-surface) }
  article { min-width:0 }
  .stat-card { --card-surface:#e3eddc; border-color:#b8c9b0 }
  form { display:grid; gap:10px; margin:15px 15px 0; padding:14px }
  label { display:grid; gap:4px; font-size:.8rem }
  select,input { width:100%; box-sizing:border-box; padding:6px }
  .heading { padding:5px 8px 5px 14px; gap:2px }
  .name { min-width:0; flex:1; text-align:left; padding:7px 4px 7px 0; border:0; background:transparent; font-size:14px }
  .name strong { flex:1; overflow-wrap:anywhere }
  .brief { font-variant-numeric:tabular-nums; color:var(--card-accent); overflow-wrap:anywhere }
  .chevron { flex:none; width:6px; height:6px; border:solid var(--card-muted); border-width:0 1px 1px 0; transform:rotate(45deg) }
  .name[aria-expanded="false"] .chevron { transform:rotate(-45deg) }
  .body { padding:0 14px 4px; overflow:auto }
  details { position:relative }
  summary { list-style:none; padding:5px; color:var(--card-muted); border-radius:4px }
  summary::-webkit-details-marker { display:none }
  .menu { position:absolute; z-index:2; right:0; top:100%; min-width:110px; padding:4px; background:#fffef8; box-shadow:0 5px 18px #263b251c }
  .menu button { display:block; width:100%; background:transparent; border:0; text-align:left }
  .menu button:hover,summary:hover,.add:hover { background:#dce6d7 }
  .name:hover strong { color:var(--card-accent) }
  footer,small,.stack>p { font-size:11px; color:var(--card-muted) }
  footer { padding:5px 14px 12px }
  p[role=alert] { margin-inline:15px; overflow-wrap:anywhere }
</style>
