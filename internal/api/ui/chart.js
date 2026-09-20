"use strict";
const pressureChart = {
  canvas: document.getElementById("pd-chart"),
  tooltip: document.getElementById("pd-chart-tooltip"),
  history: [], hover: null, geometry: null
};
const pressureSeries = [
  {role:"decode",label:"Decode",color:"#5c78d8",fill:"rgba(92,120,216,0.13)",dash:[]},
  {role:"prefill",label:"Prefill",color:"#16826c",fill:"rgba(22,130,108,0.09)",dash:[5,4]}
];
const pressureValue = (point, role) => {
  const value=point?.[role]?.pressure;
  return typeof value==="number" && Number.isFinite(value) && value>=0 ? value : null;
};
function renderPressureChart(history) {
  pressureChart.history=history.filter(point=>Number.isFinite(Date.parse(point.at)));
  drawPressureChart();
}
function drawPressureChart() {
  const {canvas,tooltip,history}=pressureChart, rect=canvas.getBoundingClientRect();
  if(!rect.width || !rect.height) return;
  const w=rect.width,h=rect.height,dpr=window.devicePixelRatio||1;
  canvas.width=Math.round(w*dpr);canvas.height=Math.round(h*dpr);
  const ctx=canvas.getContext("2d");ctx.setTransform(dpr,0,0,dpr,0,0);
  const left=43,right=w-12,top=12,bottom=h-29;
  const end=history.length?Date.parse(history[history.length-1].at):Date.now(),start=end-300000;
  const visible=history.filter(point=>Date.parse(point.at)>=start && Date.parse(point.at)<=end);
  const peak=Math.max(1,...visible.flatMap(p=>pressureSeries.map(s=>pressureValue(p,s.role)||0)));
  const max=Math.ceil(peak*4)/4;
  const x=point=>left+(Date.parse(point.at)-start)/300000*(right-left),y=value=>bottom-value/max*(bottom-top);
  pressureChart.geometry={left,right,top,bottom,start,end};
  ctx.font='11px "Segoe UI", "Microsoft YaHei", sans-serif';ctx.lineWidth=1;
  for(let i=0;i<=4;i++) {
    const value=max*i/4,py=y(value);
    ctx.strokeStyle=i===0?"#dfe8e3":"#edf1ee";ctx.setLineDash(i===0?[]:[3,5]);
    ctx.beginPath();ctx.moveTo(left,py);ctx.lineTo(right,py);ctx.stroke();
    ctx.fillStyle="#94a39a";ctx.textAlign="right";ctx.fillText(Math.round(value*100)+"%",left-10,py+4);
  }
  ctx.setLineDash([]);
  const tickCount=w<460?3:5;
  for(let i=0;i<=tickCount;i++) {
    const px=left+(right-left)*i/tickCount,date=new Date(start+300000*i/tickCount);
    ctx.fillStyle="#94a39a";ctx.textAlign=i===0?"left":i===tickCount?"right":"center";
    ctx.fillText(date.toLocaleTimeString("zh-CN",{hour12:false,hour:"2-digit",minute:"2-digit"}),px,h-6);
  }
  ctx.save();ctx.beginPath();ctx.rect(left-4,top-5,right-left+8,bottom-top+10);ctx.clip();
  for(const series of pressureSeries) {
    // Missing samples and long collection gaps stay visibly disconnected.
    const segments=[];let segment=[];
    for(const point of visible) {
      if(pressureValue(point,series.role)===null) {if(segment.length)segments.push(segment);segment=[];continue;}
      if(segment.length && Date.parse(point.at)-Date.parse(segment[segment.length-1].at)>15000) {segments.push(segment);segment=[];}
      segment.push(point);
    }
    if(segment.length)segments.push(segment);
    for(const points of segments) {
      const gradient=ctx.createLinearGradient(0,top,0,bottom);gradient.addColorStop(0,series.fill);gradient.addColorStop(1,"rgba(255,255,255,0)");
      const line=()=>{ctx.beginPath();points.forEach((p,i)=>ctx[i?"lineTo":"moveTo"](x(p),y(pressureValue(p,series.role))));};
      line();ctx.lineTo(x(points[points.length-1]),bottom);ctx.lineTo(x(points[0]),bottom);ctx.closePath();ctx.fillStyle=gradient;ctx.fill();
      line();ctx.strokeStyle=series.color;ctx.lineWidth=2;ctx.lineJoin="round";ctx.lineCap="round";ctx.setLineDash(series.dash);ctx.stroke();
      if(points.length===1) {ctx.beginPath();ctx.arc(x(points[0]),y(pressureValue(points[0],series.role)),2.5,0,Math.PI*2);ctx.fillStyle=series.color;ctx.fill();}
    }
  }
  ctx.restore();ctx.setLineDash([]);
  if(!visible.some(p=>pressureSeries.some(s=>pressureValue(p,s.role)!==null))) {
    ctx.textAlign="center";ctx.fillStyle="#94a39a";ctx.font='12px "Segoe UI", "Microsoft YaHei", sans-serif';
    ctx.fillText("等待负载数据",(left+right)/2,(top+bottom)/2);
  }
  tooltip.hidden=true;
  if(pressureChart.hover===null || !visible.length) return;
  const point=visible.reduce((best,p)=>Math.abs(Date.parse(p.at)-pressureChart.hover)<Math.abs(Date.parse(best.at)-pressureChart.hover)?p:best);
  const px=x(point);
  ctx.strokeStyle="#b5c3bb";ctx.lineWidth=1;ctx.setLineDash([3,4]);ctx.beginPath();ctx.moveTo(px,top);ctx.lineTo(px,bottom);ctx.stroke();ctx.setLineDash([]);
  tooltip.replaceChildren();const time=document.createElement("strong");time.textContent=new Date(point.at).toLocaleTimeString("zh-CN",{hour12:false});tooltip.append(time);
  for(const series of [...pressureSeries].reverse()) {
    const value=pressureValue(point,series.role),row=document.createElement("div"),label=document.createElement("span"),number=document.createElement("b");
    label.textContent=series.label;label.className=series.role==="prefill"?"tooltip-p":"tooltip-d";
    number.textContent=value===null?"无数据":(value*100).toFixed(1)+"%";row.append(label,number);tooltip.append(row);
    if(value!==null) {ctx.beginPath();ctx.arc(px,y(value),3.5,0,Math.PI*2);ctx.fillStyle=series.color;ctx.fill();ctx.strokeStyle="#fff";ctx.lineWidth=2;ctx.stroke();}
  }
  tooltip.hidden=false;
  tooltip.style.left=Math.max(0,Math.min(w-tooltip.offsetWidth,px>w*.6?px-tooltip.offsetWidth-12:px+12))+"px";
  tooltip.style.top="18px";
}
pressureChart.canvas.addEventListener("pointermove",event=>{
  const g=pressureChart.geometry;if(!g)return;
  const px=event.clientX-pressureChart.canvas.getBoundingClientRect().left;
  pressureChart.hover=g.start+Math.max(0,Math.min(1,(px-g.left)/(g.right-g.left)))*300000;drawPressureChart();
});
function clearPressureHover() {pressureChart.hover=null;drawPressureChart();}
pressureChart.canvas.addEventListener("pointerleave",clearPressureHover);
pressureChart.canvas.addEventListener("blur",clearPressureHover);
pressureChart.canvas.addEventListener("keydown",event=>{
  if(!["ArrowLeft","ArrowRight","Escape"].includes(event.key))return;
  event.preventDefault();if(event.key==="Escape"){clearPressureHover();return;}
  const h=pressureChart.history;if(!h.length)return;
  let index=pressureChart.hover===null?h.length-1:h.reduce((best,p,i)=>Math.abs(Date.parse(p.at)-pressureChart.hover)<Math.abs(Date.parse(h[best].at)-pressureChart.hover)?i:best,0);
  index=Math.max(0,Math.min(h.length-1,index+(event.key==="ArrowLeft"?-1:1)));
  pressureChart.hover=Date.parse(h[index].at);drawPressureChart();
});
new ResizeObserver(drawPressureChart).observe(pressureChart.canvas);
window.addEventListener("resize",drawPressureChart);
