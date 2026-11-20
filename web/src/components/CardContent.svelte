<script lang="ts">
  import type { Content, Kind, Point } from "./workspace-cards";
  let {kind, data, source, onSelect}: {kind: Kind; data: Content; source: string; onSelect: (id:string)=>Promise<void>} = $props();
  const palette = ["#427a65", "#bb7951", "#d4b26e", "#c5c9bd", "#85749c", "#6b889a", "#a46e78"];
  const fillId = $props.id();
  const unit = $derived(source === "running" ? "resources" : source === "tasks" || source === "running-tasks" ? "tasks" : "");
  let items = $derived(data.items ?? []);
  let total = $derived(items.reduce((sum,item) => sum + (item.value ?? 0), 0));
  let gradient = $derived.by(() => {
    let current = 0;
    return items.map((item,index) => {
      const start = current;
      current += (item.value ?? 0) * 100 / total;
      return `${palette[index % palette.length]} ${start}% ${current}%`;
    }).join(",");
  });
  const percent = $derived(data.max ? Math.round((data.value ?? 0) * 100 / data.max) : null);
  function coordinates(points: Point[]) {
    const times = points.map(point => Date.parse(point.time));
    const values = points.map(point => point.value);
    const low = Math.min(...values), high = Math.max(...values);
    return points.map((point,index) => ({
      x: 3 + (times[index]-times[0]) / (times.at(-1)!-times[0] || 1) * 254,
      y: high === low ? 27 : 49-(point.value-low)/(high-low)*44,
      gap: index > 0 && times[index]-times[index-1] > 60000,
    }));
  }
  const dots = $derived(coordinates(data.points ?? []));
  const line = $derived(dots.map((dot,index) => `${index===0 || dot.gap ? "M":"L"}${dot.x} ${dot.y}`).join(" "));
  const area = $derived(dots.map((dot, index) => {
    const start = index === 0 || dot.gap ? `M${dot.x} 54 ` : "";
    const end = index === dots.length - 1 || dots[index + 1].gap ? ` L${dot.x} 54 Z` : "";
    return `${start}L${dot.x} ${dot.y}${end}`;
  }).join(" "));
</script>

{#if kind === "stat" || kind === "trend"}
  <div class:stat={kind === "stat"} class:trend={kind === "trend"}>
    <div class="readout"><strong class="number">{data.value ?? data.points?.at(-1)?.value ?? "—"}</strong>{#if unit}<small>{unit}</small>{/if}</div>
    {#if dots.length}
      <svg viewBox="0 0 260 54" preserveAspectRatio="none" role="img" aria-label="Observed values over time">
        <defs><linearGradient id={fillId} x2="0" y2="1"><stop stop-color="currentColor" stop-opacity=".2"/><stop offset="1" stop-color="currentColor" stop-opacity="0"/></linearGradient></defs>
        <path d={area} fill={`url(#${fillId})`} />
        <path d={line} fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round" vector-effect="non-scaling-stroke" />
        {#each dots as dot,index}{#if index===0 || dot.gap || index===dots.length-1}<circle cx={dot.x} cy={dot.y} r="2" fill="currentColor" />{/if}{/each}
      </svg>
      {#if kind === "trend"}<div class="range"><span>{new Date(data.points![0].time).toLocaleTimeString()}</span><span>{new Date(data.points!.at(-1)!.time).toLocaleTimeString()}</span></div>{/if}
    {:else if kind === "trend"}<small>Waiting for samples</small>{/if}
  </div>
{:else if kind === "gauge"}
  {#if percent === null}<small>No tasks</small>{:else}
    <div class="gauge">
      <div><strong class="number">{percent}<em>%</em></strong><small>{data.value} / {data.max}</small></div>
      <div class="arc" style:--angle={`${180*(data.value ?? 0)/data.max!}deg`} role="meter" aria-label="Gauge value" aria-valuemin={0} aria-valuemax={data.max} aria-valuenow={data.value}></div>
    </div>
  {/if}
{:else if kind === "pie"}
  {#if total > 0}
    <div class="distribution"><div class="pie" style:background={`conic-gradient(${gradient})`}><b title={`Total: ${total}`}>{total}</b></div>
      <ul>{#each items as item,index}<li><i style:background={palette[index % palette.length]}></i>{item.label.replaceAll("_"," ")} <b>{item.value ?? 0}</b></li>{/each}</ul>
    </div>
  {:else}<small>No distribution yet</small>{/if}
{:else if kind === "list"}
  <ul class="items">{#each items as item}<li>{#if item.resource}<button onclick={() => onSelect(item.resource!)}>{item.label}</button>
    {:else if item.url}<a href={item.url} target="_blank" rel="noopener noreferrer">{item.label}</a>{:else}{item.label}{/if}</li>
    {:else}<li><small>No items</small></li>{/each}</ul>
{:else if kind === "html"}
  <iframe title="Published card content" sandbox="" referrerpolicy="no-referrer" srcdoc={`<!doctype html><meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; form-action 'none'; base-uri 'none'"><style>body{font:14px/1.5 system-ui;margin:0;color:#24392c;background:#fffef8}h1,h2,h3,p,ul{margin:.3em 0}table{border-collapse:collapse;width:100%}th,td{border-bottom:1px solid #d6d9cc;padding:5px;text-align:left}</style>${data.html ?? ""}`}></iframe>
{/if}
<style>
  .number { font:500 34px/1 Georgia,serif; letter-spacing:-.055em; font-variant-numeric:tabular-nums; overflow-wrap:anywhere }
  .number em { font-size:.65em; font-style:normal }
  small,.range { color:var(--card-muted); font-size:11px }
  .readout { display:flex; flex-wrap:wrap; align-items:baseline; gap:5px; min-width:0 }
  .stat,.gauge,.distribution { display:flex; align-items:center; justify-content:space-between; gap:14px }
  .stat { padding-block:8px }
  .stat .number { font-size:clamp(40px,14cqi,57px) }
  svg { display:block; color:var(--card-accent); width:100%; height:60px }
  .stat svg { flex:0 1 42%; min-width:35px; height:48px }
  .trend svg { margin-top:9px; background:repeating-linear-gradient(0deg,transparent 0 19px,var(--card-line) 19px 20px) }
  .range { display:flex; justify-content:space-between; font-variant-numeric:tabular-nums }
  .gauge small { display:block; margin-top:3px }
  .arc { position:relative; width:76px; height:38px; overflow:hidden; flex:none }
  .arc::before { content:""; position:absolute; inset:0 0 auto; aspect-ratio:1; border-radius:50%; background:conic-gradient(from 270deg,var(--card-accent) 0 var(--angle),var(--card-line) var(--angle) 180deg,transparent 180deg); mask:radial-gradient(circle,transparent 53%,black 55%) }
  .distribution { padding-block:6px }
  .pie { flex:none; width:80px; aspect-ratio:1; border-radius:50%; display:grid; place-items:center }
  .pie b { width:58px; aspect-ratio:1; border-radius:50%; display:grid; place-items:center; background:var(--card-surface); font:600 16px/1.1 system-ui; overflow:hidden; text-overflow:ellipsis }
  ul { list-style:none; padding:0; margin:0 }
  li { font-size:13px; overflow-wrap:anywhere }
  .distribution ul { flex:1; max-width:160px; min-width:0 }
  .distribution li { display:flex; align-items:center; gap:7px; padding:2px 0 }
  .distribution li b { margin-left:auto }
  i { flex:none; width:7px; height:7px; border-radius:50% }
  .items li { padding-block:6px }
  .items li + li { border-top:1px solid var(--card-line) }
  li button,li a { width:100%; background:none; border:0; padding:2px 0; color:inherit; display:flex; align-items:baseline; justify-content:space-between; gap:8px; font:inherit; text-align:left; cursor:pointer; text-decoration:none }
  li button::after,li a::after { content:"↗" / ""; color:var(--card-accent) }
  li button:hover,li a:hover { color:var(--card-accent); text-decoration:underline }
  iframe { width:100%; border:0; min-height:8em; background:var(--card-surface) }
</style>
