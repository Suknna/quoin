import { mountWorkbench } from './main'

const root = document.getElementById('root')
if (!root) throw new Error('Workbench root is missing')
mountWorkbench(root)
