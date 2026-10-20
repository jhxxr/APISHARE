/* Local, version-pinned Lobe Icons artwork. No CDN or UI framework at runtime. */
(() => {
  const root = "/public/static/brand/model-icons-1.95.1/";
  const versions = {"openai.svg":"a595df6b423920c67a7f8f73","claude-color.svg":"a3101f3047a119aa11825ad9","anthropic.svg":"e833fdfa7e7187a86a05b870","gemini-color.svg":"8ab0a9bafec11f7e69bcb9fc","google-color.svg":"4e6aa8892ef15a6b431f40f2","deepseek-color.svg":"deba5f98a5c1796e20fcac31","qwen-color.svg":"77f5768c66d08ce1d3d14e73","zai.svg":"e748cb5108ce37b116d7a5ba","moonshot.svg":"435f41b74e6a87639b6bf860","kimi-color.svg":"74a7292aeb0220445d14c5d3","minimax-color.svg":"7f7187fa6d9b341ac5f22f6c","grok.svg":"9175fc90c22655160231976c","xai.svg":"89eb7de9f0d02a41cfecd910","mistral-color.svg":"722f74b289d95486b43662fe","meta-color.svg":"adf9f2c1a646ccd3a37ca8c2","doubao-color.svg":"3cd31ba03ae44b8c29561597","bytedance-color.svg":"3d37647be262c79b044e293b","hunyuan-color.svg":"510e7bb438506b5aadfaf8b9","baichuan-color.svg":"7d49311206402734850ed9dc","yi-color.svg":"d78b1ce0eeb7a7d1a6132785","cohere-color.svg":"84d0ee3cbe66f030e5a18cb2","perplexity-color.svg":"8353f3ab20822f1a933224b0","stepfun-color.svg":"f55afc0e6004c8854f76bd5b","nvidia-color.svg":"8c941e4eb8b782eccaaea124","cline.svg":"9003d2cdce82b016147b732a","opencode.svg":"7cfa6e9d6726f7c9fa26c7d9"};
  const definitions = {
    openai: ["OpenAI", "openai.svg"], claude: ["Claude", "claude-color.svg"], anthropic: ["Anthropic", "anthropic.svg"],
    gemini: ["Gemini", "gemini-color.svg"], google: ["Google", "google-color.svg"], deepseek: ["DeepSeek", "deepseek-color.svg"],
    qwen: ["Qwen", "qwen-color.svg"], zai: ["Z.ai / GLM", "zai.svg"], moonshot: ["Moonshot", "moonshot.svg"], kimi: ["Kimi", "kimi-color.svg"],
    minimax: ["MiniMax", "minimax-color.svg"], grok: ["Grok", "grok.svg"], xai: ["xAI", "xai.svg"], mistral: ["Mistral", "mistral-color.svg"],
    meta: ["Meta / Llama", "meta-color.svg"], doubao: ["Doubao", "doubao-color.svg"], bytedance: ["ByteDance", "bytedance-color.svg"],
    hunyuan: ["Hunyuan", "hunyuan-color.svg"], baichuan: ["Baichuan", "baichuan-color.svg"], yi: ["Yi", "yi-color.svg"],
    cohere: ["Cohere", "cohere-color.svg"], perplexity: ["Perplexity", "perplexity-color.svg"], stepfun: ["StepFun", "stepfun-color.svg"],
    nvidia: ["NVIDIA", "nvidia-color.svg"], cline: ["Cline", "cline.svg"], opencode: ["OpenCode", "opencode.svg"],
  };
  const catalog = Object.fromEntries(Object.entries(definitions).map(([id, [name, file]]) => [id, Object.freeze({id, name, src: root + file + "?v=" + versions[file]})]));
  const rules = [
    ["claude", /(?:^|[/:\s])(?:claude|anthropic)(?:[-_.\s]|$)/i],
    ["gemini", /(?:^|[/:\s])(?:gemini|gemma)(?:[-_.\s\d]|$)/i],
    ["deepseek", /(?:^|[/:\s])deepseek(?:[-_.\s]|$)/i],
    ["qwen", /(?:^|[/:\s])(?:qwen|qwq|qvq)(?:[-_.\s\d]|$)/i],
    ["kimi", /(?:^|[/:\s])kimi(?:[-_.\s]|$)/i],
    ["moonshot", /(?:^|[/:\s])moonshot(?:[-_.\s]|$)/i],
    ["zai", /(?:^|[/:\s])(?:glm|chatglm|z\.ai|zai)(?:[-_.\s\d]|$)/i],
    ["minimax", /(?:^|[/:\s])(?:minimax|abab)(?:[-_.\s\d]|$)/i],
    ["grok", /(?:^|[/:\s])grok(?:[-_.\s]|$)/i],
    ["mistral", /(?:^|[/:\s])(?:mistral|mixtral|codestral|devstral|ministral|pixtral|magistral|voxtral)(?:[-_.\s\d]|$)/i],
    ["meta", /(?:^|[/:\s])(?:meta-)?llama(?:[-_.\s\d]|$)/i],
    ["doubao", /(?:^|[/:\s])doubao(?:[-_.\s]|$)/i],
    ["bytedance", /(?:^|[/:\s])(?:seed|seedream|seedance)(?:[-_.\s\d]|$)/i],
    ["hunyuan", /(?:^|[/:\s])hunyuan(?:[-_.\s]|$)/i],
    ["baichuan", /(?:^|[/:\s])baichuan(?:[-_.\s\d]|$)/i],
    ["yi", /(?:^|[/:\s])yi(?:[-_.\s\d]|$)/i],
    ["cohere", /(?:^|[/:\s])(?:command|cohere)(?:[-_.\s]|$)/i],
    ["perplexity", /(?:^|[/:\s])(?:sonar|perplexity)(?:[-_.\s]|$)/i],
    ["stepfun", /(?:^|[/:\s])(?:step|stepfun)(?:[-_.\s\d]|$)/i],
    ["nvidia", /(?:^|[/:\s])(?:nemotron|nvidia)(?:[-_.\s]|$)/i],
    ["openai", /(?:^|[/:\s])(?:gpt|chatgpt|codex|o[1-9]|dall-?e|sora|whisper|text-embedding|tts|davinci|babbage)(?:[-_.\s\d]|$)/i],
  ];
  const families = {openai:"openai", anthropic:"claude", claude:"claude", google:"gemini", gemini:"gemini", deepseek:"deepseek", qwen:"qwen", glm:"zai", zhipu:"zai", "z.ai":"zai", zai:"zai", moonshot:"moonshot", kimi:"kimi", minimax:"minimax", xai:"grok", grok:"grok", mistral:"mistral", meta:"meta", llama:"meta", doubao:"doubao", bytedance:"bytedance", hunyuan:"hunyuan", yi:"yi", cohere:"cohere", perplexity:"perplexity", stepfun:"stepfun", nvidia:"nvidia"};
  const escape = (value) => String(value ?? "").replace(/[&<>"']/g, (ch) => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[ch]));
  const fallback = '<svg viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round"><path d="m12 3 9 5-9 5-9-5 9-5Zm-9 9 9 5 9-5M3 16l9 5 9-5"/></svg>';
  function modelBrand(model, family = "") {
    // Bedrock and other catalogs qualify names with dots/underscores as well
    // as slashes (e.g. us.anthropic.claude-sonnet-4). Keep model rules first
    // so a protocol/provider prefix never overrides the actual model brand.
    const text = String(model || ""), qualified = text.replace(/[._]/g, " ");
    for (const [brand, pattern] of rules) if (pattern.test(text) || pattern.test(qualified)) return catalog[brand];
    return catalog[families[String(family).trim().toLowerCase()]] || null;
  }
  function providerBrand(type, baseURL = "") {
    let host = "";
    try { host = new URL(baseURL).hostname.toLowerCase(); } catch { /* Custom or empty endpoint. */ }
    const hosts = {"deepseek.com":"deepseek","dashscope.aliyuncs.com":"qwen","dashscope-intl.aliyuncs.com":"qwen","bigmodel.cn":"zai","z.ai":"zai","moonshot.cn":"moonshot","moonshot.ai":"moonshot","minimax.io":"minimax","minimaxi.com":"minimax","x.ai":"xai","mistral.ai":"mistral","api.siliconflow.cn":"generic"};
    for (const [domain, brand] of Object.entries(hosts)) if (host === domain || host.endsWith("." + domain)) return catalog[brand] || null;
    return catalog[{openai:"openai",anthropic:"anthropic",gemini:"gemini",cline:"cline",opencode:"opencode","opencode-free":"opencode"}[type]] || null;
  }
  function markup(brand) {
    return '<span class="provider-icon model-brand-icon" data-brand="' + (brand?.id || "generic") + '" aria-hidden="true" title="' + escape(brand?.name || "模型") + '">' + (brand ? '<img src="' + brand.src + '" width="24" height="24" alt="" decoding="async" data-brand-icon>' : fallback) + '</span>';
  }
  function labelHTML(model, family = "") {
    return '<span class="model-identity-label">' + markup(modelBrand(model, family)) + '<span class="model-label-text">' + escape(model || "未识别模型") + '</span></span>';
  }
  window.APIShareModelIcons = Object.freeze({catalog:Object.freeze(catalog), modelBrand, providerBrand, labelHTML, html:(model, family) => markup(modelBrand(model, family)), providerHTML:(type, baseURL) => markup(providerBrand(type, baseURL))});
  document.addEventListener("error", (event) => {
    const target = event.target;
    if (target instanceof HTMLImageElement && target.hasAttribute("data-brand-icon")) {
      const parent = target.closest(".model-brand-icon");
      if (parent) { parent.innerHTML = fallback; parent.dataset.brand = "generic"; }
    }
  }, true);
})();
