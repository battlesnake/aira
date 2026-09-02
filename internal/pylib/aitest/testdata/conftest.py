import importlib
import os
import sys

aira_py_lib = os.environ["AIRA_AITEST_LIB"]
if aira_py_lib not in sys.path:
    sys.path.insert(0, aira_py_lib)
importlib.import_module("aitest")
pytest_plugins = ("aitest",)
