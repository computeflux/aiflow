declare module "simple-mind-map" {
  const MindMap: any;
  export default MindMap;
}

declare module "simple-mind-map-plugin-themes" {
  const themes: {
    init: (MindMap: any) => void;
  };
  export default themes;
}
