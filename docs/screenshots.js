// Regenerates docs/map.png + docs/story.png: run a throwaway container with a fresh DB on :8097
// (ADMIN_USERNAME=demo ADMIN_PASSWORD=demo-pass-1234), then `OUT=docs node docs/screenshots.js`
// from a directory with playwright installed. Seeds three fictional trips on first run.
const { chromium } = require('playwright');
const EXE='/home/giedrius/.cache/ms-playwright/chromium_headless_shell-1234/chrome-headless-shell-linux64/chrome-headless-shell';
const BASE='http://localhost:8097', OUT=process.env.OUT||'.';
const trips=[
 {name:'Scottish Highlands',color:'#2e7d4f',start:'2025-05-12',end:'2025-05-18',pins:[
   [56.682,-5.104,'Glencoe','The Three Sisters in full cloud drama.','2025-05-12T14:00:00Z'],
   [57.412,-6.196,'Isle of Skye','Portree harbour, then the Old Man of Storr.','2025-05-14T11:00:00Z'],
   [57.478,-4.225,'Inverness','Last dram by the River Ness.','2025-05-16T19:00:00Z'],
   [55.953,-3.188,'Edinburgh','One more night, on the Royal Mile.','2025-05-18T17:00:00Z']]},
 {name:'Lisbon',color:'#1d6fb8',start:'2024-10-03',end:'2024-10-08',pins:[
   [38.712,-9.130,'Alfama','Tram 28 and pastéis for breakfast.','2024-10-04T10:00:00Z'],
   [38.797,-9.390,'Sintra','Pena Palace in the mist.','2024-10-06T12:00:00Z']]},
 {name:'Amalfi Coast',color:'#c0392b',start:'2025-06-20',end:'2025-06-27',pins:[
   [40.852,14.268,'Naples','Pizza at Da Michele, twice.','2025-06-21T13:00:00Z'],
   [40.628,14.485,'Positano','Steps, lemons, the sea.','2025-06-24T16:00:00Z']]},
];
(async()=>{
 const b=await chromium.launch({executablePath:EXE});
 const ctx=await b.newContext({viewport:{width:1280,height:800},deviceScaleFactor:2,serviceWorkers:'block',colorScheme:'light'});
 const p=await ctx.newPage();
 await p.goto(BASE,{waitUntil:'networkidle'});
 await p.fill('#login-user','demo'); await p.fill('#login-pass','demo-pass-1234');
 await p.click('#login-form button[type=submit]'); await p.waitForSelector('#login',{state:'hidden'});
 const api=(m,u,body)=>p.evaluate(async([m,u,body])=>{const r=await fetch(u,{method:m,headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});return [r.status,await r.text()]},[m,u,body]);
 const existing=await p.evaluate(()=>fetch('/api/holidays').then(r=>r.json()));
 if(!existing.length){
  for(const t of trips){
   const [s,txt]=await api('POST','/api/holidays',{name:t.name,color:t.color,start_at:t.start,end_at:t.end});
   if(s!==200&&s!==201) throw new Error('holiday '+s+' '+txt);
   const h=JSON.parse(txt);
   for(const [lat,lng,title,note,at] of t.pins){
     const [ps,pt]=await api('POST','/api/pins',{holiday_id:h.id,lat,lng,title,note,created_at:at});
     if(ps!==200&&ps!==201) throw new Error('pin '+ps+' '+pt);
   }
  }
  await p.reload({waitUntil:'networkidle'}); await p.waitForTimeout(1500);
 }
 // 1. the atlas: Europe, sheet collapsed
 await p.evaluate(()=>{ map.setView([49.2,8.5],5,{animate:false}); });
 await p.waitForTimeout(3500);
 await p.screenshot({path:OUT+'/map.png'});
 // 2. a trip story, opened at Skye
 const hs=await p.evaluate(()=>fetch('/api/holidays').then(r=>r.json()));
 const h=hs.find(x=>x.name==='Scottish Highlands');
 const pins=await p.evaluate(()=>state.pins);
 const skye=pins.find(x=>x.title==='Glencoe');
 await p.evaluate(([h,id])=>openStory(h,id),[h,skye.id]);
 await p.waitForTimeout(3500);
 await p.screenshot({path:OUT+'/story.png'});
 await b.close();
})().catch(e=>{console.error('ERR',e.message);process.exit(1)});
