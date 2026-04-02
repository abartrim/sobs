#!/usr/bin/env node
/**
 * Build an ECharts series-type catalog from the installed ECharts typings.
 * Usage: node scripts/extract-echarts-types.js
 */

const fs = require("fs");
const path = require("path");

const optionTypesPath = path.join(
  __dirname,
  "..",
  "node_modules",
  "echarts",
  "types",
  "src",
  "export",
  "option.d.ts"
);

const METADATA = {
  line: {
    name: "Line Chart",
    description: "Trends over time or ordered categories.",
    dataStructure: {
      type: "xAxis + yAxis with series data array",
      example: "xAxis: ['Jan'], series: [{ type: 'line', data: [120] }]",
    },
    goodFor: "Time series, trends",
    exampleIds: ["line-simple", "line-smooth"],
  },
  bar: {
    name: "Bar Chart",
    description: "Compare values across categories.",
    dataStructure: {
      type: "xAxis + yAxis with series data array",
      example: "xAxis: ['A'], series: [{ type: 'bar', data: [42] }]",
    },
    goodFor: "Category comparisons",
    exampleIds: ["bar-simple", "bar-y-category"],
  },
  scatter: {
    name: "Scatter Plot",
    description: "Relationship between numeric variables.",
    dataStructure: {
      type: "Array of [x, y] pairs",
      example: "series: [{ type: 'scatter', data: [[10, 20], [20, 30]] }]",
    },
    goodFor: "Correlation analysis",
    exampleIds: ["scatter-simple"],
  },
  pie: {
    name: "Pie Chart",
    description: "Part-to-whole composition.",
    dataStructure: {
      type: "Array of {name, value} objects",
      example: "series: [{ type: 'pie', data: [{name: 'A', value: 10}] }]",
    },
    goodFor: "Composition, percentages",
    exampleIds: ["pie-simple", "pie-donut"],
  },
  radar: {
    name: "Radar Chart",
    description: "Compare multiple dimensions on radial axes.",
    dataStructure: {
      type: "radar.indicator + values arrays",
      example: "radar: {indicator:[{name:'Q1', max:100}]}, series:[{type:'radar', data:[[80]]}]",
    },
    goodFor: "Multi-attribute comparison",
    exampleIds: ["radar", "radar-multiple-series"],
  },
  map: {
    name: "Map",
    description: "Geographic visualization by region.",
    dataStructure: {
      type: "Array of {name, value} region data",
      example: "series: [{ type: 'map', map: 'world', data: [{name: 'Canada', value: 50}] }]",
    },
    goodFor: "Geo distribution",
    exampleIds: ["map-world", "map-usa"],
  },
  tree: {
    name: "Tree Diagram",
    description: "Hierarchical parent-child structure.",
    dataStructure: {
      type: "Nested nodes with children",
      example: "series: [{ type: 'tree', data: [{name: 'root', children: [{name: 'child'}]}] }]",
    },
    goodFor: "Hierarchies",
    exampleIds: ["tree", "tree-orient"],
  },
  treemap: {
    name: "Treemap",
    description: "Hierarchical values as nested rectangles.",
    dataStructure: {
      type: "Nested {name, value, children}",
      example: "series: [{ type: 'treemap', data: [{name:'A', value: 10}] }]",
    },
    goodFor: "Space-efficient hierarchy",
    exampleIds: ["treemap-simple"],
  },
  graph: {
    name: "Graph Network",
    description: "Node-link network relationships.",
    dataStructure: {
      type: "nodes + links arrays",
      example: "series: [{ type: 'graph', data: [{name:'A'}], links:[{source:'A',target:'B'}] }]",
    },
    goodFor: "Networks, relationship maps",
    exampleIds: ["graph", "graph-simple"],
  },
  chord: {
    name: "Chord Diagram",
    description: "Circular flow/relationship view between categories.",
    dataStructure: {
      type: "nodes + matrix/edges depending on option style",
      example: "series: [{ type: 'chord', data: [], links: [] }]",
    },
    goodFor: "Inter-category flow relationships",
    exampleIds: [],
  },
  gauge: {
    name: "Gauge",
    description: "Single metric against a scale.",
    dataStructure: {
      type: "Array of {name, value}",
      example: "series: [{ type: 'gauge', data: [{name:'Score', value: 78}] }]",
    },
    goodFor: "KPI display",
    exampleIds: ["gauge", "gauge-speed-chart"],
  },
  funnel: {
    name: "Funnel",
    description: "Stage-based conversion drop-off.",
    dataStructure: {
      type: "Array of {name, value}",
      example: "series: [{ type: 'funnel', data: [{name:'Visit', value:1000}] }]",
    },
    goodFor: "Conversion analysis",
    exampleIds: ["funnel", "funnel-sort"],
  },
  parallel: {
    name: "Parallel Coordinates",
    description: "High-dimensional row-wise data.",
    dataStructure: {
      type: "parallelAxis config + row arrays",
      example: "series: [{ type: 'parallel', data: [[1,2,3],[2,3,4]] }]",
    },
    goodFor: "Multi-dimensional analysis",
    exampleIds: ["parallel-aqi"],
  },
  sankey: {
    name: "Sankey",
    description: "Weighted flow between nodes.",
    dataStructure: {
      type: "nodes + links with source/target/value",
      example: "series: [{ type: 'sankey', nodes:[{name:'A'}], links:[{source:'A',target:'B',value:10}] }]",
    },
    goodFor: "Flow diagrams",
    exampleIds: ["sankey-energy"],
  },
  boxplot: {
    name: "Boxplot",
    description: "Distribution summary (min/Q1/median/Q3/max).",
    dataStructure: {
      type: "Array of five-number summary arrays",
      example: "series: [{ type: 'boxplot', data: [[850, 940, 980, 1070, 1170]] }]",
    },
    goodFor: "Distribution and outlier analysis",
    exampleIds: ["boxplot-light-velocity"],
  },
  candlestick: {
    name: "Candlestick",
    description: "OHLC financial series.",
    dataStructure: {
      type: "Array of [open, close, low, high]",
      example: "series: [{ type: 'candlestick', data: [[20, 34, 10, 38]] }]",
    },
    goodFor: "Financial charts",
    exampleIds: ["candlestick-sh"],
  },
  effectScatter: {
    name: "Effect Scatter",
    description: "Scatter with animated ripple effects.",
    dataStructure: {
      type: "Array of [x, y] pairs",
      example: "series: [{ type: 'effectScatter', data: [[10, 20], [30, 50]] }]",
    },
    goodFor: "Highlight key points",
    exampleIds: ["effectScatter"],
  },
  lines: {
    name: "Lines",
    description: "Line segments between coordinates or geo points.",
    dataStructure: {
      type: "Array of line objects with coords or source/target",
      example: "series: [{ type: 'lines', data: [{coords:[[120,30],[121,31]]}] }]",
    },
    goodFor: "Routes, movement paths",
    exampleIds: ["lines-airline"],
  },
  heatmap: {
    name: "Heatmap",
    description: "Intensity matrix over 2D space.",
    dataStructure: {
      type: "Array of [x, y, value]",
      example: "series: [{ type: 'heatmap', data: [[0,0,5],[1,0,8]] }]",
    },
    goodFor: "Density and pattern detection",
    exampleIds: ["heatmap", "heatmap-cartesian"],
  },
  pictorialBar: {
    name: "Pictorial Bar",
    description: "Bar chart using symbols/icons.",
    dataStructure: {
      type: "Like bar with symbol options",
      example: "series: [{ type: 'pictorialBar', data: [120, 90] }]",
    },
    goodFor: "Stylized comparisons",
    exampleIds: ["pictorialBar"],
  },
  themeRiver: {
    name: "Theme River",
    description: "Category streams evolving over time.",
    dataStructure: {
      type: "Array of [time, value, category]",
      example: "series: [{ type: 'themeRiver', data: [[" + "'2024-01-01'" + ", 10, 'A']] }]",
    },
    goodFor: "Category trend streams",
    exampleIds: ["themeRiver"],
  },
  sunburst: {
    name: "Sunburst",
    description: "Hierarchical radial composition.",
    dataStructure: {
      type: "Nested {name, value, children}",
      example: "series: [{ type: 'sunburst', data: [{name:'root', children:[{name:'leaf', value:1}]}] }]",
    },
    goodFor: "Hierarchy in radial form",
    exampleIds: ["sunburst", "sunburst-simple"],
  },
  custom: {
    name: "Custom Series",
    description: "Fully custom rendering with renderItem.",
    dataStructure: {
      type: "Arbitrary data + custom renderItem function",
      example: "series: [{ type: 'custom', renderItem: function(){}, data: [] }]",
    },
    goodFor: "Bespoke visual encodings",
    exampleIds: ["custom-cartesian-polygon"],
  },
};

const ALIASES = {
  area: {
    mapsTo: "line",
    name: "Area Chart (Alias)",
    hint: "Use line series with areaStyle: {}",
  },
  bubble: {
    mapsTo: "scatter",
    name: "Bubble Chart (Alias)",
    hint: "Use scatter and set symbolSize from third value.",
  },
};

function extractRegisteredSeriesTypes() {
  if (!fs.existsSync(optionTypesPath)) {
    throw new Error(`ECharts option typings not found: ${optionTypesPath}`);
  }
  const src = fs.readFileSync(optionTypesPath, "utf8");
  const ifaceMatch = src.match(/export interface RegisteredSeriesOption \{([\s\S]*?)\n\}/);
  if (!ifaceMatch) {
    throw new Error("Could not find RegisteredSeriesOption in ECharts typings.");
  }

  const body = ifaceMatch[1];
  const keys = [];
  const keyRegex = /^\s*([A-Za-z][A-Za-z0-9_]*)\s*:/gm;
  let match = keyRegex.exec(body);
  while (match) {
    keys.push(match[1]);
    match = keyRegex.exec(body);
  }

  if (!keys.length) {
    throw new Error("No series types extracted from RegisteredSeriesOption.");
  }
  return keys;
}

function buildCatalogChartTypes(seriesTypes) {
  const chartTypes = {};
  for (const type of seriesTypes) {
    const meta = METADATA[type] || {};
    chartTypes[type] = {
      name: meta.name || `${type} series`,
      description: meta.description || "ECharts series type.",
      dataStructure: meta.dataStructure || {
        type: "Depends on series configuration",
        example: `series: [{ type: '${type}', data: [] }]`,
      },
      configTemplate: {
        type,
        series: [{ type, data: [] }],
      },
      goodFor: meta.goodFor || "Type-specific visual encoding",
      examplesGalleryUrl: "https://echarts.apache.org/examples/",
      exampleIds: meta.exampleIds || [],
    };
  }
  return chartTypes;
}

function writePythonCatalog(pythonOutputPath, chartTypes, generatedAt) {
  const pythonHeader = [
    "# Auto-generated ECharts chart types catalog",
    "# flake8: noqa: E501",
    `# Generated: ${generatedAt}`,
    "# Source: scripts/extract-echarts-types.js",
    "",
  ].join("\n");

  const jsonLike = JSON.stringify(chartTypes, null, 2)
    .replace(/\btrue\b/g, "True")
    .replace(/\bfalse\b/g, "False")
    .replace(/\bnull\b/g, "None");

  const pythonContent = `${pythonHeader}ECHARTS_CHART_TYPES = ${jsonLike}\n`;
  fs.writeFileSync(pythonOutputPath, pythonContent);
}

function generateCatalog() {
  const generatedAt = new Date().toISOString();
  const seriesTypes = extractRegisteredSeriesTypes();
  const chartTypes = buildCatalogChartTypes(seriesTypes);

  const catalog = {
    version: "6.0.0",
    generatedAt,
    source: "RegisteredSeriesOption",
    totalSeriesTypes: seriesTypes.length,
    seriesTypes,
    aliases: ALIASES,
    chartTypes,
  };

  const jsonOutputPath = path.join(__dirname, "..", "static", "echarts-chart-types.json");
  const jsonDir = path.dirname(jsonOutputPath);
  if (!fs.existsSync(jsonDir)) {
    fs.mkdirSync(jsonDir, { recursive: true });
  }
  fs.writeFileSync(jsonOutputPath, JSON.stringify(catalog, null, 2));
  console.log(`✓ Chart types catalog generated: ${jsonOutputPath}`);
  console.log(`✓ Extracted ${seriesTypes.length} official ECharts series types`);

  const pythonOutputPath = path.join(__dirname, "..", "echarts_chart_types.py");
  writePythonCatalog(pythonOutputPath, chartTypes, generatedAt);
  console.log(`✓ Python chart types catalog generated: ${pythonOutputPath}`);
}

try {
  generateCatalog();
  console.log("\n✨ ECharts catalog extraction complete!");
} catch (error) {
  console.error("Error generating catalog:", error.message);
  process.exit(1);
}
