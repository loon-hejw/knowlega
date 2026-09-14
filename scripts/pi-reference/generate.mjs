import { readFile, writeFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';
const root=process.env.PI_REFERENCE_ROOT;
const core=await import(root?pathToFileURL(`${root}/@earendil-works/pi-agent-core/dist/index.js`).href:'@earendil-works/pi-agent-core');
const ai=await import(root?pathToFileURL(`${root}/@earendil-works/pi-ai/dist/index.js`).href:'@earendil-works/pi-ai');
const cases=JSON.parse(await readFile(new URL('../../internal/qm/agent/testdata/pi-loop-cases.json',import.meta.url),'utf8'));
const output=[];
for(const c of cases){
 let step=0; const executed=[]; const events=[];
 const model={id:'mock',name:'mock',api:'openai-completions',provider:'openai',baseUrl:'https://example.invalid',reasoning:false,input:['text'],contextWindow:65536,maxTokens:4096,cost:{input:0,output:0,cacheRead:0,cacheWrite:0}};
 const tool={name:'knowledge',label:'knowledge',description:'test',parameters:{type:'object',properties:{action:{type:'string'}},required:['action']},execute:async(id,args)=>{executed.push(id);return{content:[{type:'text',text:c.payload}],details:{},isError:!!c.failed}}};
 const messages=await core.runAgentLoop([{role:'user',content:'question',timestamp:0}],{systemPrompt:'',messages:[],tools:[tool]}, {model,convertToLlm:m=>m,toolExecution:'sequential'},e=>events.push(e.type),undefined,()=>{
  const frame=c.completions[step++]; if(!frame)throw new Error('unexpected model call');
  const message={role:'assistant',api:model.api,provider:model.provider,model:model.id,timestamp:0,stopReason:frame.stopReason||(frame.calls?'toolUse':'stop'),content:[...(frame.text?[{type:'text',text:frame.text}]:[]),...(frame.calls||[]).map(x=>({type:'toolCall',id:x.id,name:'knowledge',arguments:{action:x.action}}))],usage:{input:0,output:0,cacheRead:0,cacheWrite:0,totalTokens:0,cost:{input:0,output:0,cacheRead:0,cacheWrite:0,total:0}}};
  const stream=new ai.EventStream(e=>e.type==='done'||e.type==='error',e=>e.message||e.error);
  queueMicrotask(()=>{stream.push({type:'start',partial:message});stream.push({type:'done',reason:message.stopReason,message});stream.end(message)});return stream;
 });
 output.push({name:c.name,modelCalls:step,executed,reply:messages.at(-1).content.filter(x=>x.type==='text').map(x=>x.text).join(''),events});
}
await writeFile(new URL('../../internal/qm/agent/testdata/pi-loop-reference.json',import.meta.url),JSON.stringify({version:'0.85.1',commit:'d981de1229ef899957bbe968bc8dcda02a21f477',cases:output},null,2)+'\n');
console.log(`Generated ${output.length} reference traces from TS Pi 0.85.1`);
