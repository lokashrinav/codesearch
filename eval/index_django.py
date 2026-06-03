"""Index Django repo with Python extractor."""
import sys, os
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'cmd', 'experiment9'))
from extract_python import index_python_repo

stats = index_python_repo(r'C:\Users\lokas\django', 'django.db')
print(f"Indexed: {stats['files']} files, {stats['idents']} idents, {stats['edges']} edges in {stats['time']:.2f}s")
