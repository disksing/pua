import {describe, expect, it} from "vitest";
import {collapsedReadout, project, quotaEntryKey, quotaStatus, quotaValue, summarize, visibleQuotaProviders, type Card} from "../../src/components/workspace-cards";

describe("workspace card sources", () => {
  const tree = {projects:[{id:"project1",type:"project",children:[
    {id:"project1.task1",type:"task",state:"waiting" as const},
    {id:"project1.task2",type:"task",runtime:{activeTurn:true}},
    {id:"project1.task3",type:"task",archived:true,runtime:{activeTurn:true}},
  ]}], scheduler:{id:"scheduler",runtime:{activeTurn:true}}};
  const summary = summarize(tree);
  const card:Card = {kind:"stat",source:"running",id:"sample",title:"Running",version:1,collapsed:false};
  it("counts direct matches, excludes archives and keeps task state separate", () => {
    expect(project(card,summary,[])?.value).toBe(2);
    expect(project({...card,kind:"list"},summary,[])?.items?.map(item=>item.resource)).toEqual(["scheduler","project1.task2"]);
    expect(project({...card,kind:"gauge"},summary,[])).toEqual({value:1,max:2});
    expect(project({...card,kind:"pie"},summary,[])?.items).toContainEqual({label:"waiting",value:1});
  });
  it("distinguishes unavailable data and leaves unsampled history empty", () => {
    expect(project(card,null,[])).toBeNull();
    expect(project({...card,kind:"trend"},summary,[])?.points).toEqual([]);
    expect(project({...card,source:"agent",content:{value:9}},null,[])?.value).toBe(9);
  });
  it("shows a compact value when the card is collapsed", () => {
    expect(collapsedReadout("stat", {value:0})).toBe("0");
    expect(collapsedReadout("trend", null)).toBe("—");
    expect(collapsedReadout("gauge", {value:1,max:4})).toBe("25%");
    expect(collapsedReadout("list", {items:[]})).toBe("0");
    expect(collapsedReadout("pie", {items:[{label:"Running",value:3}]})).toBe("3");
    expect(collapsedReadout("html", {html:"<p>Ready</p>"})).toBe("");
  });
  it("distinguishes quota availability, stale readings and measured zero", () => {
    const snapshot = {configured:true, connected:true, providers:[]};
    expect(quotaStatus({...snapshot, configured:false, connected:false})).toBe("Magpie not configured");
    expect(quotaStatus({...snapshot, connected:false})).toBe("Magpie disconnected");
    expect(quotaStatus({...snapshot, stale:true})).toBe("No quota readings");
    expect(quotaStatus(snapshot)).toBe("No quota readings");
    expect(quotaValue({label:"Weekly", remainingPercent:null})).toBe("Unavailable");
    expect(quotaValue({label:"Weekly", remainingPercent:0})).toBe("0% left");
    expect(quotaValue({label:"Credits", remainingPercent:null, display:"$12"})).toBe("$12");
    expect(collapsedReadout("quota", {})).toBe("Unavailable");
  });
  it("filters matching window keys per account and preserves empty provider readings", () => {
    const weekly = {key:"weekly", label:"Weekly", remainingPercent:50};
    const first = {id:"first", label:"Provider", status:"ok", quotas:[weekly]};
    const second = {...first, id:"second"};
    const quota = {configured:true, connected:true, providers:[first, second]};
    expect(quotaStatus({...quota, stale:true})).toBe("");
    const hideFirst = [quotaEntryKey(first, weekly)];
    const hideBoth = [...hideFirst, quotaEntryKey(second, weekly)];
    expect(visibleQuotaProviders(quota, hideFirst)).toEqual([second]);
    expect(collapsedReadout("quota", {quota}, hideBoth)).toBe("All entries hidden");
    expect(collapsedReadout("quota", {quota})).toBe("2 accounts");
    const empty = {...first, quotas:[]};
    expect(visibleQuotaProviders({...quota, providers:[empty]}, hideBoth)).toEqual([empty]);
  });
});
