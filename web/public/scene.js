/* Slow ink-like contours, independent of API traffic and service health. */
(() => {
 const canvas=document.getElementById("particleScene"),screen=document.getElementById("firstScreen"),ctx=canvas?.getContext("2d",{alpha:true});
 if(!ctx||!screen)return;
 let width=0,height=0,dpr=1,frame=0,last=0,phase=.7,elapsed=0;
 const entering=document.documentElement.classList.contains("intro-active"),running=()=>window.pageMotion?.isRunning()===true;
 function render() {
  if(!width||!height)return;
  const motion=window.pageMotion?.scene()||{progress:0,x:0,y:0};
  const birth=entering&&document.documentElement.classList.contains("intro-active")?Math.min(1,elapsed/1800):1;
  const formed=1-(1-birth)**3,sway=Math.sin(phase*.6),breath=Math.cos(phase*.42);
  const driftX=motion.x*2,driftY=motion.y*2,entry=(1-formed)*height*.2;
  ctx.clearRect(0,0,width,height);ctx.save();ctx.translate(driftX,driftY);
  ctx.globalAlpha=.9*formed;
  const bands=width<600?44:68;
  // Lower-left contours sweep out of the frame like layered sheets of paper.
  const lower=ctx.createLinearGradient(0,height*.5,width,height);
  lower.addColorStop(0,"rgba(174,102,68,.34)");lower.addColorStop(.45,"rgba(191,126,83,.22)");lower.addColorStop(1,"rgba(197,156,98,.08)");
  for(let i=0;i<bands;i++) {
   const t=i/(bands-1),wave=Math.sin(t*4+phase)*.025;
   ctx.beginPath();
   ctx.moveTo(-width*.12,height*(.43+t*.25)+entry);
   ctx.bezierCurveTo(width*(.04+sway*.04),height*(1.08+t*.13+wave)+entry,width*(.46+breath*.055),height*(.70+t*.27)+entry,width*1.12,height*(.88+t*.30+wave)+entry);
   ctx.strokeStyle=lower;ctx.lineWidth=i%5===0?1.15:.65;ctx.stroke();
  }
  // A second, lighter family enters from the upper-right corner.
  const upper=ctx.createLinearGradient(0,0,width,height*.45);
  upper.addColorStop(0,"rgba(195,155,98,.08)");upper.addColorStop(.65,"rgba(180,119,83,.18)");upper.addColorStop(1,"rgba(170,101,72,.34)");
  for(let i=0;i<bands;i++) {
   const t=i/(bands-1),wave=Math.cos(t*4-phase*.8)*.025;
   ctx.beginPath();ctx.moveTo(-width*.12,height*(-.27+t*.19)-entry);
   ctx.bezierCurveTo(width*(.50+sway*.035),height*(-.14+t*.18)-entry,width*(.82+breath*.04),height*(.46+t*.12+wave)-entry,width*1.14,height*(.14+t*.28)-entry);
   ctx.strokeStyle=upper;ctx.lineWidth=i%5===0?1:.65;ctx.stroke();
  }
  ctx.restore();
  // Leave the numerical readout quiet and high-contrast at every aspect ratio.
  ctx.save();ctx.globalCompositeOperation="destination-out";ctx.translate(width*.5,height*.46);ctx.scale(width*.58,height*.29);
  const clear=ctx.createRadialGradient(0,0,.12,0,0,1);clear.addColorStop(0,"rgba(0,0,0,1)");clear.addColorStop(.6,"rgba(0,0,0,.9)");clear.addColorStop(1,"rgba(0,0,0,0)");
  ctx.fillStyle=clear;ctx.fillRect(-1,-1,2,2);ctx.restore();
 }
 function resize(){width=screen.clientWidth;height=screen.clientHeight;dpr=Math.min(devicePixelRatio||1,1.5);canvas.width=Math.round(width*dpr);canvas.height=Math.round(height*dpr);ctx.setTransform(dpr,0,0,dpr,0,0);render();sync();}
 function tick(now){frame=0;if(!running())return;const delta=now-last;if(delta>=32){const advance=last?Math.min(delta,64):32;phase+=advance*.00024;elapsed+=advance;last=now;render();}frame=requestAnimationFrame(tick);}
 function sync(){if(running()){if(!frame){last=0;frame=requestAnimationFrame(tick);}}else{cancelAnimationFrame(frame);frame=0;render();}}
 window.addEventListener("public-motion-change",sync);
 if("ResizeObserver" in window)new ResizeObserver(resize).observe(screen);else window.addEventListener("resize",resize,{passive:true});
 resize();
})();
