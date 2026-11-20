import {describe, expect, it} from "vitest";
import {collapsedReadout, project, summarize, type Card} from "../../src/components/workspace-cards";

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
});
