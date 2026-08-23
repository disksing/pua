import { mount } from "svelte";

import AgentHubApp from "./AgentHubApp.svelte";
import "./agenthub.css";

const target = document.getElementById("app");
if (!target) throw new Error("AgentHub application root is missing");

mount(AgentHubApp, { target });
